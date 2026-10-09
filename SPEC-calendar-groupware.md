# Specification: Exchange-like Groupware Calendar Features

## Overview
Implement Microsoft Outlook-style groupware calendar capabilities centered on two models:
- **Shared Calendars** — owned by an individual, shared with granular permissions
- **Group Calendars** — owned by a team/workgroup, membership-driven access

Plus: rich meeting invites, Scheduling Assistant with Room Finder, response tracking, and delegation.

---

## 1. Data Model Extensions

### 1.1 Calendar Sharing (Personal → User/Group)
```sql
-- New table: calendar_shares
CREATE TABLE calendar_shares (
    calendar_id  bigint NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    -- Target is either a user or a workgroup
    target_type  text NOT NULL CHECK (target_type IN ('user', 'workgroup')),
    target_user  citext REFERENCES users(address) ON DELETE CASCADE,
    target_wg    bigint REFERENCES workgroups(id) ON DELETE CASCADE,
    -- Permission level
    permission   text NOT NULL CHECK (permission IN (
        'freebusy',           -- Can view when busy (free/busy/tentative/away)
        'titles_locations',   -- Can view titles and locations
        'details',            -- Can view all details
        'editor',             -- Can edit events
        'delegate'            -- Editor + receives meeting requests
    )),
    -- Who granted this share
    granted_by   citext NOT NULL REFERENCES users(address),
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (calendar_id, target_type, target_user, target_wg),
    -- Exactly one target
    CONSTRAINT calendar_shares_one_target CHECK (
        (target_type = 'user'   AND target_user IS NOT NULL AND target_wg IS NULL)
        OR
        (target_type = 'workgroup' AND target_wg IS NOT NULL AND target_user IS NULL)
    )
);

CREATE INDEX calendar_shares_target_user_idx ON calendar_shares (target_user);
CREATE INDEX calendar_shares_target_wg_idx ON calendar_shares (target_wg);
```

### 1.2 Calendar Delegates (Personal → User)
```sql
-- New table: calendar_delegates
CREATE TABLE calendar_delegates (
    calendar_id  bigint NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    delegate     citext NOT NULL REFERENCES users(address) ON DELETE CASCADE,
    -- Can the delegate see private events?
    can_view_private boolean NOT NULL DEFAULT false,
    -- Does the delegate receive meeting requests?
    receives_requests boolean NOT NULL DEFAULT true,
    granted_by   citext NOT NULL REFERENCES users(address),
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (calendar_id, delegate)
);
```

### 1.3 Meeting Invite Extensions (events table additions)
```sql
-- Add columns to events table
ALTER TABLE events ADD COLUMN IF NOT EXISTS hide_attendee_list boolean NOT NULL DEFAULT false;
ALTER TABLE events ADD COLUMN IF NOT EXISTS block_forwarding boolean NOT NULL DEFAULT false;
ALTER TABLE events ADD COLUMN IF NOT EXISTS request_responses boolean NOT NULL DEFAULT true;
ALTER TABLE events ADD COLUMN IF NOT EXISTS attached_files jsonb;  -- [{name, cid, mime, size}]
ALTER TABLE events ADD COLUMN IF NOT EXISTS poll_data jsonb;       -- Microsoft Forms poll
```

### 1.4 Room Resources (for Room Finder)
```sql
-- New table: rooms
CREATE TABLE rooms (
    id           bigserial PRIMARY KEY,
    email        citext UNIQUE NOT NULL,      -- room@domain
    display_name text NOT NULL,
    capacity     int NOT NULL DEFAULT 0,
    location     text,                        -- Building/Floor
    description  text,
    timezone     text NOT NULL DEFAULT 'UTC',
    -- Auto-accept settings
    auto_accept  boolean NOT NULL DEFAULT true,
    -- Booking window (days ahead)
    booking_window_days int NOT NULL DEFAULT 365,
    -- Recurring booking limits
    max_recurrence_instances int NOT NULL DEFAULT 52,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Room calendar mapping (each room has a calendar)
ALTER TABLE rooms ADD COLUMN calendar_id bigint UNIQUE REFERENCES calendars(id) ON DELETE SET NULL;

-- Room delegates (who can manage the room)
CREATE TABLE room_delegates (
    room_id  bigint NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    delegate citext NOT NULL REFERENCES users(address) ON DELETE CASCADE,
    PRIMARY KEY (room_id, delegate)
);
```

---

## 2. Permission Model

### 2.1 Permission Levels (Hierarchical)
| Level | Free/Busy | Titles & Locations | Full Details | Edit Events | Receive Requests |
|-------|-----------|-------------------|--------------|-------------|------------------|
| `freebusy` | ✓ | | | | |
| `titles_locations` | ✓ | ✓ | | | |
| `details` | ✓ | ✓ | ✓ | | |
| `editor` | ✓ | ✓ | ✓ | ✓ | |
| `delegate` | ✓ | ✓ | ✓ | ✓ | ✓ |

### 2.2 Effective Permission Calculation
```
User's effective permission on calendar = MAX(
    direct share permission,
    workgroup membership permission (if workgroup calendar),
    delegate permission (if delegate),
    owner permission (full)
)
```

### 2.3 Policy Actions (Extend calendar.go)
```go
// New actions
ActionViewCalendarFreeBusy     = "view_calendar_freebusy"
ActionViewCalendarTitles       = "view_calendar_titles"
ActionViewCalendarDetails      = "view_calendar_details"
ActionEditCalendar             = "edit_calendar"
ActionDelegateCalendar         = "delegate_calendar"
ActionManageRoom               = "manage_room"
```

---

## 3. API Endpoints

### 3.1 Calendar Sharing
```
GET    /api/v1/calendars/{id}/shares          -- List shares
POST   /api/v1/calendars/{id}/shares          -- Create share
PATCH  /api/v1/calendars/{id}/shares/{target} -- Update share
DELETE /api/v1/calendars/{id}/shares/{target} -- Remove share
```

### 3.2 Calendar Delegates
```
GET    /api/v1/calendars/{id}/delegates       -- List delegates
POST   /api/v1/calendars/{id}/delegates       -- Add delegate
PATCH  /api/v1/calendars/{id}/delegates/{u}   -- Update delegate
DELETE /api/v1/calendars/{id}/delegates/{u}   -- Remove delegate
```

### 3.3 Room Management (Admin)
```
GET    /api/v1/admin/rooms                    -- List rooms
POST   /api/v1/admin/rooms                    -- Create room
PATCH  /api/v1/admin/rooms/{id}               -- Update room
DELETE /api/v1/admin/rooms/{id}               -- Delete room
GET    /api/v1/admin/rooms/{id}/calendar      -- Get room's calendar
POST   /api/v1/admin/rooms/{id}/delegates     -- Add room delegate
DELETE /api/v1/admin/rooms/{id}/delegates/{u} -- Remove room delegate
```

### 3.4 Enhanced Event Creation
```
POST /api/v1/calendars/{id}/events
{
  "event": { ... },
  "options": {
    "hideAttendeeList": false,
    "blockForwarding": false,
    "requestResponses": true,
    "attachments": [{"name": "...", "content": "base64", "mime": "..."}],
    "poll": {"question": "...", "options": ["A","B"]}
  }
}
```

### 3.5 Scheduling Assistant
```
POST /api/v1/calendars/freebusy
{
  "addresses": ["user1@domain", "room@domain"],
  "start": "2026-10-15T09:00:00Z",
  "end": "2026-10-15T17:00:00Z"
}
→ Returns busy blocks per address for timeline rendering
```

### 3.6 Room Finder
```
GET /api/v1/rooms?start=...&end=...&capacity=10
→ Returns available rooms with their free/busy in the window
```

---

## 4. iTIP/iMIP Enhancements

### 4.1 Extended Organizer Properties
- `X-MICROSOFT-HIDE-ATTENDEE-LIST: TRUE`
- `X-MICROSOFT-BLOCK-FORWARDING: TRUE`
- `X-MICROSOFT-RESPONSE-REQUESTED: FALSE`

### 4.2 Attendee Properties
- `CN` (display name)
- `ROLE` (REQ-PARTICIPANT, OPT-PARTICIPANT, CHAIR)
- `PARTSTAT` (NEEDS-ACTION, ACCEPTED, DECLINED, TENTATIVE, DELEGATED)
- `RSVP` (TRUE/FALSE)
- `DELEGATED-FROM` / `DELEGATED-TO`

### 4.3 Response Tracking
- Store attendee `PARTSTAT` in `attendees` table
- Webhook/notification on response receipt
- Organizer sees aggregate counts in event detail

---

## 5. UI Components

### 5.1 Calendar List (Left Sidebar)
- Personal calendars (owned)
- Shared with me (permission badge)
- Group/Workgroup calendars
- Room calendars (filterable)
- Context menu: Share, Delegate, Permissions, Delete

### 5.2 Share Dialog
```
┌─────────────────────────────────────┐
│ Share Calendar                      │
├─────────────────────────────────────┤
│ Add people/groups: [search input]   │
│ ┌─────────────────────────────────┐ │
│ │ user@domain      ▼ Can edit     │ │
│ │ team@domain      ▼ Can view     │ │
│ └─────────────────────────────────┘ │
│ [Cancel]                    [Save]  │
└─────────────────────────────────────┘
```

### 5.3 Delegate Dialog
- Add delegate → checkboxes: "Receive meeting requests", "View private events"

### 5.4 Event Composer (Enhanced)
- "Options" panel: Hide attendee list, Block forwarding, Request responses
- "Attach files" → upload → shows as inline or attachment
- "Add poll" → Microsoft Forms integration
- Room picker (from Room Finder)

### 5.5 Scheduling Assistant View
- Horizontal timeline per attendee/room
- Color-coded: Free (green), Busy (red), Tentative (yellow), OOF (purple)
- "AutoPick" button → suggests best slot
- Drag meeting block to adjust

### 5.6 Room Finder
- Filter by capacity, location, equipment
- Shows availability in selected time window
- "Add room" → adds as attendee with room email

### 5.7 Event Detail — Tracking Tab
- Table: Attendee | Status | Response Time
- "Resend invite" button per attendee

### 5.8 Delegation Settings (per calendar)
- List delegates with permission toggles
- "Private events visible to delegate" checkbox

---

## 6. Server-Side Implementation Plan

### Phase 1: Database & Core Sharing
1. Migrations for `calendar_shares`, `calendar_delegates`, `rooms`, room columns
2. Policy actions + `CanReadCalendar`, `CanEditCalendar`, `CanDelegateCalendar`
3. `store` methods for shares/delegates
4. HTTP handlers for share/delegate CRUD

### Phase 2: Meeting Invite Extensions
1. Event columns: `hide_attendee_list`, `block_forwarding`, `request_responses`
2. iTIP property serialization
3. Response tracking in `attendees` table

### Phase 4: Room Resources
1. `rooms` table + admin CRUD
2. Auto-accept logic (CalDAV scheduling inbox)
4. Room Finder API

### Phase 5: Scheduling Assistant
1. Free/busy aggregation for multiple calendars + rooms
2. Timeline rendering data structure
2. AutoPick algorithm (find first slot where all free)

### Phase 6: UI Integration
1. Calendar list with sharing UI
2. Enhanced event composer
3. Scheduling Assistant panel
4. Room Finder modal
4. Tracking tab in event detail
5. Delegation settings page

---

## 7. Testing Requirements

### 7.1 Unit Tests
- Permission resolution (user + workgroup + delegate + owner)
- Share/delegate CRUD
- iTIP property round-trip

### 7.2 Integration Tests
- Create share → verify free/busy visibility
- Delegate receives meeting request
- Room auto-accept/decline
- Scheduling Assistant returns correct busy blocks

### 7.3 E2E Tests
- User A shares calendar with User B (editor) → B creates event
- Organizer creates meeting with room → room auto-accepts
- Scheduling Assistant finds slot for 5 people + room
- Response tracking updates in real-time

---

## 8. Configuration

```yaml
calendar:
  # Maximum attendees per meeting
  max_attendees: 500
  # Maximum recurrence instances
  max_recurrence: 730
  # Default booking window for rooms (days)
  room_booking_window_days: 365
  # Auto-decline conflicts for rooms
  room_auto_decline_conflicts: true
  # Allow external attendees
  allow_external_attendees: true
```

---

## 9. Migration Strategy

1. Add tables/columns (backward compatible)
2. Deploy server with new policy actions (no-op initially)
3. Enable sharing UI
4. Enable delegate UI
6. Enable room management
7. Enable Scheduling Assistant
8. Enable tracking UI

---

## 10. Open Questions

1. **External attendees** — Full iMIP email flow or just calendar.ics attachment?
2. **Poll integration** — Microsoft Forms API or built-in simple polls?
3. **Loop components** — Out of scope for v1?
4. **Mobile push** — Calendar notifications via existing push infrastructure?
5. **Time zone handling** — Per-attendee time zone in Scheduling Assistant?
6. **Calendar groups** — "My Calendars", "Shared with me", "Rooms" grouping in UI?