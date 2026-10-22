#!/usr/bin/env python3
"""Live interoperability check for linchange meetings.

Drives the whole meeting lifecycle the way real clients would, asserting every
wire surface with independent, spec-derived tooling - the Python standard
library only. Nothing here imports linchange code or reuses the server's
codecs:

  1.  create a meeting over the HTTP API (one local guest, one external)
  2.  read the local guest's invitation from their IMAP inbox and parse the
      MIME with the stdlib email package: text/calendar part, method=REQUEST
      parameter, METHOD:REQUEST body, per-guest tokenised RSVP link
  3.  read the external guest's exact invitation bytes from the blob store
      (addressed via the outbox row) and parse them the same way
  4.  answer as the external guest through the public, token-gated view - no
      session, exactly like the link in their invitation - and confirm the
      organiser sees the answer
  5.  send a chat message into the meeting's native room as the organiser and
      read it back as the guest
  6.  have the local guest accept by email (iTIP METHOD:REPLY over SMTP) and
      confirm the organiser's view reflects it - the inbound iTIP hook
  7.  resend, then cancel, and confirm the CANCEL mail and the delivery-failure
      report (DSN) for the undeliverable external address

Run against the e2e stack:  bash web/e2e/stack.sh  (or the manual boot), then
    python3 e2e/meetings/meetings_e2e.py
"""

import email
import email.policy
import imaplib
import json
import re
import smtplib
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timedelta, timezone
from pathlib import Path

BASE = "http://127.0.0.1:18080"
API = BASE + "/api/v1"
IMAP_HOST, IMAP_PORT = "127.0.0.1", 1143
SMTP_HOST, SMTP_PORT = "127.0.0.1", 1025
DB_CONTAINER = "linchange-postgres"
DB_NAME = "linchange_e2e"

ORG = "e2e-user@example.com"          # organiser, signed in below
GUEST = "e2e-other@example.com"       # local guest with a mailbox
EXTERNAL = "carol@outside.example"    # external guest: no mailbox, no MX
PASSWORD = "correct horse battery staple"

REPO = Path(__file__).resolve().parents[2]
TOKEN_RE = re.compile(r"to=([^&\s]+)&token=([0-9a-f]{32})")


def fail(what, detail=""):
    print(f"FAIL - {what}" + (f": {detail}" if detail else ""))
    sys.exit(1)


def ok(what):
    print(f"ok   - {what}")


# --------------------------------------------------------------------------
# HTTP
# --------------------------------------------------------------------------

def api(method, path, body=None, token=None, quiet=False):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(API + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return json.loads(resp.read())
    except urllib.error.HTTPError as e:
        if not quiet:
            detail = e.read()[:300].decode(errors="replace")
            print(f"[{method} {path} -> {e.code}] {detail}")
        raise


def api_expect(method, path, body, token, want_status):
    try:
        api(method, path, body, token, quiet=True)
    except urllib.error.HTTPError as e:
        if e.code == want_status:
            return
        fail(f"{method} {path} -> {e.code}, expected {want_status}")
    fail(f"{method} {path} -> 2xx, expected {want_status}")


# --------------------------------------------------------------------------
# IMAP / SMTP / blob store
# --------------------------------------------------------------------------

def inbox_raw(user, limit=40):
    con = imaplib.IMAP4(IMAP_HOST, IMAP_PORT)
    try:
        con.login(user, PASSWORD)
        con.select("INBOX")
        _, data = con.search(None, "ALL")
        ids = data[0].split()[-limit:]
        out = []
        for mid in ids:
            _, fetched = con.fetch(mid, "(RFC822)")
            out.append(fetched[0][1])
        return out
    finally:
        try:
            con.logout()
        except Exception:
            pass


def parse(raw):
    return email.message_from_bytes(raw, policy=email.policy.default)


def calendar_parts(msg):
    return [p for p in msg.walk() if p.get_content_type() == "text/calendar"]


def find_invitation(uid, user):
    """The invitation for this meeting, from this user's inbox.

    Delivery is asynchronous and the mailbox can already hold invitations for
    older meetings, so the match is by the event's UID inside the calendar
    part rather than by subject or arrival order.
    """
    for raw in reversed(inbox_raw(user)):
        msg = parse(raw)
        if not (msg["Subject"] or "").startswith("Invitation:"):
            continue
        for part in calendar_parts(msg):
            if f"UID:{uid}" in part.get_content():
                return msg
    return None


def ics_text(part):
    """The calendar document with RFC 5545 line folding undone.

    Content lines longer than 75 octets continue on the next line after a
    CRLF and one whitespace character. A guest's ATTENDEE line is longer than
    that almost immediately, so without unfolding it reads as broken to any
    substring match - which is exactly how a client parser would have to
    unfold it too.
    """
    text = part.get_content()
    for continuation in ("\r\n ", "\r\n\t", "\n ", "\n\t"):
        text = text.replace(continuation, "")
    return text


def rsvp_token_for(msg, address):
    """The per-guest RSVP token, taken from the plain-text alternative."""
    for part in msg.walk():
        if part.get_content_type() == "text/plain":
            body = part.get_content()
            for m in TOKEN_RE.finditer(body):
                if urllib.parse.unquote(m.group(1)) == address:
                    return m.group(2)
    return None


def blob_raw_for_recipient(address):
    """The exact bytes queued for an outbox recipient, from the blob store."""
    sql = ("SELECT encode(o.blob_sha256,'hex') FROM outbox o "
           "JOIN outbox_recipients r ON r.outbox_id = o.id "
           f"WHERE r.address = '{address}' ORDER BY o.id DESC LIMIT 1")
    sha = subprocess.check_output(
        ["docker", "exec", DB_CONTAINER, "psql", "-U", "linchange", "-d", DB_NAME, "-tAc", sql],
        text=True,
    ).strip()
    if not re.fullmatch(r"[0-9a-f]{64}", sha or ""):
        fail(f"no outbox blob for {address}", sha)
    path = REPO / "data" / "blobs" / sha[:2] / sha[2:4] / sha
    if not path.is_file():
        fail(f"blob file missing for {address}", str(path))
    return path.read_bytes()


def poll_until(pred, timeout, interval=1.0):
    deadline = time.time() + timeout
    while time.time() < deadline:
        value = pred()
        if value:
            return value
        time.sleep(interval)
    return None


# --------------------------------------------------------------------------
# The meeting
# --------------------------------------------------------------------------

def assert_invitation(msg, uid, title, guest_address, join_url, label):
    """Every fact a client needs from an invitation, read from the MIME."""
    if not (msg["Subject"] or "").startswith("Invitation:") or title not in msg["Subject"]:
        fail(f"{label}: subject", msg["Subject"])

    parts = calendar_parts(msg)
    if len(parts) != 1:
        fail(f"{label}: expected one text/calendar part, got {len(parts)}")
    part = parts[0]

    if part.get_param("method") != "REQUEST":
        fail(f"{label}: Content-Type method parameter", str(part.get_content_type()))
    if part.get_filename() != "invitation.ics":
        fail(f"{label}: attachment filename", str(part.get_filename()))

    ics = ics_text(part)
    for want in [
        "METHOD:REQUEST",
        f"UID:{uid}",
        f"SUMMARY:{title}",
        f"ORGANIZER:mailto:{ORG}",
        f"ATTENDEE;CN=Other;PARTSTAT=NEEDS-ACTION;ROLE=REQ-PARTICIPANT;RSVP=TRUE:mailto:{GUEST}",
        f"ATTENDEE;PARTSTAT=NEEDS-ACTION;ROLE=REQ-PARTICIPANT;RSVP=TRUE:mailto:{EXTERNAL}",
        f"LOCATION:{join_url}",
        f"URL:{join_url}",
    ]:
        if want not in ics:
            fail(f"{label}: calendar body misses {want!r}")

    token = rsvp_token_for(msg, guest_address)
    if not token:
        fail(f"{label}: no RSVP link for {guest_address}")
    return token


def main():
    # --- 1. organiser signs in and creates the meeting --------------------
    login = api("POST", "/auth/login", {"login": ORG, "password": PASSWORD})
    token = login.get("access_token")
    if not token:
        fail("login returned no access_token", json.dumps(login)[:200])
    ok("organiser signed in")

    start = (datetime.now(timezone.utc) + timedelta(hours=3)).replace(microsecond=0)
    end = start + timedelta(hours=1)
    draft = {
        "title": "Interop sync",
        "description": "Live client interoperability check.",
        "start": start.isoformat().replace("+00:00", "Z"),
        "end": end.isoformat().replace("+00:00", "Z"),
        "timezone": "Europe/Berlin",
        "attendees": [
            {"address": GUEST, "name": "Other"},
            {"address": EXTERNAL},
        ],
    }
    meeting = api("POST", "/meetings/", draft, token)
    uid = meeting.get("uid", "")
    if not re.fullmatch(r"[0-9a-f]{32}@linchange", uid):
        fail("meeting uid shape", uid)
    join_url = f"{BASE}/meetings/{uid}"
    if meeting.get("joinUrl") != join_url:
        fail("joinUrl", meeting.get("joinUrl"))
    if meeting.get("inviteFailures"):
        fail("invite delivery reported failures", str(meeting["inviteFailures"]))
    if not meeting.get("chatConversation"):
        fail("no chatConversation although chat is native")
    by_addr = {a["address"]: a for a in meeting["attendees"]}
    if not by_addr[GUEST]["local"] or by_addr[EXTERNAL]["local"]:
        fail("local flags on attendees", json.dumps(by_addr))
    ok(f"meeting created ({uid}, room {meeting['chatConversation']})")

    # --- 2. local guest reads their invitation over IMAP ------------------
    invite = poll_until(lambda: find_invitation(uid, GUEST), 60)
    if invite is None:
        fail(f"no invitation for this meeting arrived in {GUEST}'s inbox")
    guest_token = assert_invitation(invite, uid, "Interop sync", GUEST, join_url, "IMAP invite")
    ok("local guest's invitation: method=REQUEST, METHOD:REQUEST, per-guest link")

    # --- 3. external guest's exact bytes from the blob store --------------
    ext_raw = blob_raw_for_recipient(EXTERNAL)
    ext_msg = parse(ext_raw)
    ext_token = assert_invitation(ext_msg, uid, "Interop sync", EXTERNAL, join_url, "external invite")
    if ext_token == guest_token:
        fail("per-guest RSVP tokens are identical")
    ok("external guest's queued invitation parses identically, token differs")

    # --- 4. external guest answers through the public view ----------------
    view = api("GET", f"/meeting-invites/{uid}?to={urllib.parse.quote(EXTERNAL)}&token={ext_token}")
    if view.get("chatConversation"):
        fail("public invite view leaks the chat room id")
    if len(view.get("attendees", [])) != 2:
        fail("public view guest list", json.dumps(view.get("attendees")))
    ok("public invite view: no session, no chat room, guest list intact")

    api_expect("GET", f"/meeting-invites/{uid}?to={urllib.parse.quote(EXTERNAL)}&token={'0'*32}",
               None, None, 404)
    rsvp = api("POST", f"/meeting-invites/{uid}/rsvp",
               {"to": EXTERNAL, "token": ext_token, "partStat": "ACCEPTED"})
    if rsvp.get("partStat") != "ACCEPTED":
        fail("public RSVP answer", json.dumps(rsvp))
    mine = next(a for a in api("GET", f"/meetings/{uid}", token=token)["attendees"]
                if a["address"] == EXTERNAL)
    if mine["partStat"] != "ACCEPTED":
        fail("organiser's view does not show the external answer", mine["partStat"])
    api_expect("POST", f"/meetings/{uid}/rsvp", {"partStat": "ACCEPTED"}, token, 403)
    ok("external guest answered over their token link; organiser sees ACCEPTED")

    # --- 5. native chat: the organiser writes, the guest reads -------------
    # The meeting's room is a linchange conversation. The REST path is the one
    # a client without a socket can use; the browser socket stores through the
    # same service and is exercised by the Playwright suite.
    room = meeting.get("chatConversation")
    if not room:
        fail("no chatConversation although chat is native")
    marker = f"room check {uid[:8]} {int(time.time())}"
    sent = api("POST", f"/chat/conversations/{room}/messages", {"body": marker}, token)
    if sent.get("body") != marker:
        fail("organiser's chat message was not stored", json.dumps(sent)[:200])

    guest_login = api("POST", "/auth/login", {"login": GUEST, "password": PASSWORD})
    guest_access = guest_login.get("access_token")
    if not guest_access:
        fail("guest login returned no access_token", json.dumps(guest_login)[:200])
    history = api("GET", f"/chat/conversations/{room}/messages", token=guest_access)
    if marker not in [m.get("body") for m in history.get("messages", [])]:
        fail("guest does not see the organiser's chat message", json.dumps(history)[:300])
    ok("native chat: organiser sent over HTTP, guest read it from history")

    # --- 6. local guest accepts by email; the inbound hook applies it -----
    # The reply is composed the way a real client does: it echoes the event
    # identity and timing from the received invitation, including its
    # SEQUENCE, and carries the attendee's new PARTSTAT.
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    invite_ics = ics_text(calendar_parts(find_invitation(uid, GUEST))[0])
    seq = re.search(r"SEQUENCE:(\d+)", invite_ics)
    reply = "\r\n".join([
        f"From: {GUEST}",
        f"To: {ORG}",
        "Subject: Re: Invitation: Interop sync",
        f"Date: {datetime.now(timezone.utc).strftime('%a, %d %b %Y %H:%M:%S +0000')}",
        "MIME-Version: 1.0",
        'Content-Type: text/calendar; charset="utf-8"; method=REPLY',
        "",
        "BEGIN:VCALENDAR",
        "PRODID:-//Example//Mail//EN",
        "VERSION:2.0",
        "METHOD:REPLY",
        "BEGIN:VEVENT",
        f"UID:{uid}",
        f"DTSTART:{start.strftime('%Y%m%dT%H%M%SZ')}",
        f"DTEND:{end.strftime('%Y%m%dT%H%M%SZ')}",
        f"SEQUENCE:{seq.group(1) if seq else 0}",
        f"DTSTAMP:{stamp}",
        f"ATTENDEE;CN=Other;PARTSTAT=ACCEPTED:mailto:{GUEST}",
        f"ORGANIZER;CN=Organiser:mailto:{ORG}",
        "END:VEVENT",
        "END:VCALENDAR",
    ]) + "\r\n"
    with smtplib.SMTP(SMTP_HOST, SMTP_PORT, timeout=30) as smtp:
        smtp.sendmail(GUEST, [ORG], reply)

    def guest_accepted():
        row = next((a for a in api("GET", f"/meetings/{uid}", token=token)["attendees"]
                    if a["address"] == GUEST), None)
        return row and row["partStat"] == "ACCEPTED"
    if not poll_until(guest_accepted, 30):
        fail("iTIP REPLY over SMTP never reached the meeting")
    ok("METHOD:REPLY delivered over SMTP applied by the organiser's view")

    # --- 7. resend, then cancel -------------------------------------------
    resent = api("POST", f"/meetings/{uid}/resend", {}, token)
    if resent.get("sent") != 2 or resent.get("inviteFailures"):
        fail("resend result", json.dumps(resent))

    def second_copy():
        tokens = set()
        count = 0
        for raw in inbox_raw(GUEST):
            msg = parse(raw)
            if not (msg["Subject"] or "").startswith("Invitation:"):
                continue
            for part in calendar_parts(msg):
                if f"UID:{uid}" not in part.get_content():
                    continue
                count += 1
                found = rsvp_token_for(msg, GUEST)
                if found:
                    tokens.add(found)
        return tokens if count >= 2 else None
    tokens = poll_until(second_copy, 30)
    if not tokens:
        fail("resent invitation never arrived")
    if tokens != {guest_token}:
        fail("resend changed the guest's link", str(tokens))
    ok("resend: second copy arrived, the guest's link is unchanged")

    api("POST", f"/meetings/{uid}/cancel", {}, token)

    def cancel_copy():
        for raw in inbox_raw(GUEST):
            msg = parse(raw)
            for part in calendar_parts(msg):
                if part.get_param("method") == "CANCEL":
                    ics = ics_text(part)
                    if f"UID:{uid}" in ics and "METHOD:CANCEL" in ics:
                        return True
        return None
    if not poll_until(cancel_copy, 30):
        fail("the local guest never received the CANCEL mail")

    def dsn_named():
        for raw in inbox_raw(ORG):
            msg = parse(raw)
            if (msg["Subject"] or "").startswith("Delivery Status Notification"):
                if EXTERNAL in msg.as_string():
                    return True
        return None
    if not poll_until(dsn_named, 120, interval=2.0):
        fail("no delivery-status report for the undeliverable external address")
    if not api("GET", f"/meetings/{uid}", token=token).get("cancelled"):
        fail("meeting not marked cancelled")
    api_expect("POST", f"/meetings/{uid}/resend", {}, token, 400)
    ok("cancel: CANCEL mail delivered, DSN for the external address, meeting closed")

    print("PASS - meetings live interop")


if __name__ == "__main__":
    main()
