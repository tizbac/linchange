package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// WorkgroupGroup is a named group of people inside one organization.
//
// A group exists so that a calendar share or a meeting invitation can name a
// set of people instead of an address list that goes stale the week after it is
// written down. There are two kinds of it, and the difference is the one thing
// that is easy to get backwards:
//
//   - A listed group holds exactly the addresses somebody added to it. It may
//     contain somebody from outside the organization's domains, because a
//     contractor on this week's project is a person all the same.
//   - The organization's own Everyone group, flagged rather than filled in,
//     holds every enabled mailbox in its domains. Its membership follows the
//     directory, so there is no row to keep in step as people join and leave —
//     and no row to remove while the person stays a member anyway.
type WorkgroupGroup struct {
	ID          int64  `db:"id"`
	WorkgroupID int64  `db:"workgroup_id"`
	Name        string `db:"name"`
	Description string `db:"description"`
	Everyone    bool   `db:"everyone"`
	// MemberCount is how many addresses the group expands to. It is computed
	// when the row is read rather than maintained, so it cannot be stale.
	MemberCount int       `db:"member_count"`
	CreatedAt   time.Time `db:"created_at"`
	UpdatedAt   time.Time `db:"updated_at"`
}

// Groups reads and writes the groups inside an organization. It is stateless.
type Groups struct{}

// NewGroups returns the repository.
func NewGroups() *Groups { return &Groups{} }

// groupColumns are the columns of a group row, aliased for the joined queries
// below that need them alongside a count.
const groupColumns = `g.id, g.workgroup_id, g.name, g.description, g.everyone,
	g.created_at, g.updated_at`

// listedMemberCount counts a listed group's members in the same statement that
// reads the group, so a list of groups is one query rather than one per row.
const listedMemberCount = `(SELECT count(*) FROM workgroup_group_members m WHERE m.group_id = g.id)`

// List returns an organization's groups, Everyone first and then by name.
//
// Everyone leads because it is the one that is always the right answer for
// "share with the organization", and putting it where it can be seen means an
// administrator reaches for it before inventing a group to hold the same people.
func (g *Groups) List(ctx context.Context, q Querier, workgroupID int64) ([]WorkgroupGroup, error) {
	out := make([]WorkgroupGroup, 0, 4)
	err := q.SelectContext(ctx, &out, `
		SELECT `+groupColumns+`, `+listedMemberCount+` AS member_count
		FROM workgroup_groups g
		WHERE g.workgroup_id = $1
		ORDER BY g.everyone DESC, g.name`, workgroupID)
	if err != nil {
		return nil, fmt.Errorf("store: listing workgroup groups: %w", err)
	}
	for i := range out {
		if out[i].Everyone {
			count, err := g.everyoneCount(ctx, q, workgroupID)
			if err != nil {
				return nil, err
			}
			out[i].MemberCount = count
		}
	}
	return out, nil
}

// Get returns one group.
func (g *Groups) Get(ctx context.Context, q Querier, id int64) (WorkgroupGroup, error) {
	var out WorkgroupGroup
	err := q.GetContext(ctx, &out, `
		SELECT `+groupColumns+`, `+listedMemberCount+` AS member_count
		FROM workgroup_groups g
		WHERE g.id = $1`, id)
	if err != nil {
		return WorkgroupGroup{}, mapGroupErr("getting workgroup group", err)
	}
	if out.Everyone {
		count, err := g.everyoneCount(ctx, q, out.WorkgroupID)
		if err != nil {
			return WorkgroupGroup{}, err
		}
		out.MemberCount = count
	}
	return out, nil
}

// Create adds a group to an organization.
func (g *Groups) Create(ctx context.Context, q Querier, workgroupID int64, name, description string) (WorkgroupGroup, error) {
	var out WorkgroupGroup
	err := q.GetContext(ctx, &out, `
		INSERT INTO workgroup_groups (workgroup_id, name, description)
		VALUES ($1, $2, $3)
		RETURNING id, workgroup_id, name, description, everyone, created_at, updated_at`,
		workgroupID, strings.TrimSpace(name), strings.TrimSpace(description))
	if err != nil {
		return WorkgroupGroup{}, mapGroupErr("creating workgroup group", err)
	}
	return out, nil
}

// Update changes a group's name and description. The Everyone group may be
// described but not renamed: its name is how the client recognizes it, and a
// share that names it by id should still read as "the organization" in a
// dialog somebody else is looking at.
func (g *Groups) Update(ctx context.Context, q Querier, id int64, name, description string) (WorkgroupGroup, error) {
	group, err := g.Get(ctx, q, id)
	if err != nil {
		return WorkgroupGroup{}, err
	}
	if group.Everyone {
		name = group.Name
	}
	var out WorkgroupGroup
	err = q.GetContext(ctx, &out, `
		UPDATE workgroup_groups
		SET name = $2, description = $3, updated_at = now()
		WHERE id = $1
		RETURNING id, workgroup_id, name, description, everyone, created_at, updated_at`,
		id, strings.TrimSpace(name), strings.TrimSpace(description))
	if err != nil {
		return WorkgroupGroup{}, mapGroupErr("updating workgroup group", err)
	}
	return out, nil
}

// Delete removes a group. Its shares go with it, because a calendar shared with
// a group that no longer exists has no answer to "who can see this".
func (g *Groups) Delete(ctx context.Context, q Querier, id int64) error {
	res, err := q.ExecContext(ctx, `DELETE FROM workgroup_groups WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: deleting workgroup group: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Members returns the addresses in a group.
//
// For an Everyone group the addresses are computed rather than read, so that
// this and the organization's own member list are answers to the same question
// asked the same way.
func (g *Groups) Members(ctx context.Context, q Querier, groupID int64) ([]string, error) {
	group, err := g.Get(ctx, q, groupID)
	if err != nil {
		return nil, err
	}
	if group.Everyone {
		contacts, err := NewWorkgroups().People(ctx, q, group.WorkgroupID)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(contacts))
		for _, c := range contacts {
			out = append(out, c.Address)
		}
		return out, nil
	}
	var out []string
	if err := q.SelectContext(ctx, &out, `
		SELECT address FROM workgroup_group_members WHERE group_id = $1
		ORDER BY address`, groupID); err != nil {
		return nil, fmt.Errorf("store: listing workgroup group members: %w", err)
	}
	return out, nil
}

// ForMember returns the groups an address belongs to.
//
// A listed group asks its member rows. The organization's Everyone group asks
// the directory: the address must be an enabled mailbox in one of the
// organization's domains. That is the same rule Members expands, expressed
// here so a chat room can find its groups without walking every group in the
// organization.
func (g *Groups) ForMember(ctx context.Context, q Querier, address string) ([]WorkgroupGroup, error) {
	out := make([]WorkgroupGroup, 0, 4)
	err := q.SelectContext(ctx, &out, `
		SELECT `+groupColumns+`, `+listedMemberCount+` AS member_count
		FROM workgroup_groups g
		WHERE EXISTS (
		    SELECT 1 FROM workgroup_group_members m
		    WHERE m.group_id = g.id AND m.address = $1
		) OR (
		    g.everyone AND EXISTS (
		        SELECT 1
		        FROM users u
		        WHERE u.address = $1 AND u.enabled
		          AND lower(split_part(u.address, '@', 2)) IN (
		              SELECT w.domain FROM workgroups w WHERE w.id = g.workgroup_id
		              UNION
		              SELECT d.name FROM domains d WHERE d.workgroup_id = g.workgroup_id
		          )
		    )
		)
		ORDER BY g.everyone DESC, g.name`, address)
	if err != nil {
		return nil, fmt.Errorf("store: listing a member's workgroup groups: %w", err)
	}
	for i := range out {
		if out[i].Everyone {
			count, err := g.everyoneCount(ctx, q, out[i].WorkgroupID)
			if err != nil {
				return nil, err
			}
			out[i].MemberCount = count
		}
	}
	return out, nil
}

// IsMember reports whether the address belongs to the group.
//
// It is the targeted form of Members, for callers that ask about one person
// rather than listing everyone: a listed group asks its member rows, and an
// Everyone group asks the directory with the same domain rule.
func (g *Groups) IsMember(ctx context.Context, q Querier, groupID int64, address string) (bool, error) {
	var member bool
	err := q.GetContext(ctx, &member, `
		SELECT CASE WHEN g.everyone THEN EXISTS (
		    SELECT 1
		    FROM users u
		    WHERE u.address = $2 AND u.enabled
		      AND lower(split_part(u.address, '@', 2)) IN (
		          SELECT w.domain FROM workgroups w WHERE w.id = g.workgroup_id
		          UNION
		          SELECT d.name FROM domains d WHERE d.workgroup_id = g.workgroup_id
		      )
		) ELSE EXISTS (
		    SELECT 1 FROM workgroup_group_members m
		    WHERE m.group_id = g.id AND m.address = $2
		) END
		FROM workgroup_groups g
		WHERE g.id = $1`, groupID, address)
	if IsNotFound(err) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("store: checking workgroup group membership: %w", err)
	}
	return member, nil
}

// AddMember adds an address to a listed group.
//
// An Everyone group refuses: the address is in it already if it belongs to the
// organization's domains, and a row saying so could later be taken away while
// the person stayed a member.
func (g *Groups) AddMember(ctx context.Context, q Querier, groupID int64, address string) error {
	group, err := g.Get(ctx, q, groupID)
	if err != nil {
		return err
	}
	if group.Everyone {
		return ErrConflict
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO workgroup_group_members (group_id, address)
		VALUES ($1, $2)
		ON CONFLICT (group_id, address) DO NOTHING`,
		groupID, address); err != nil {
		if IsConflict(err) {
			return fmt.Errorf("store: adding workgroup group member: %w", ErrConflict)
		}
		return fmt.Errorf("store: adding workgroup group member: %w", err)
	}
	return nil
}

// RemoveMember takes an address out of a listed group. It reports whether an
// address was there to take out, so a remove that changed nothing can be told
// apart from one that did.
func (g *Groups) RemoveMember(ctx context.Context, q Querier, groupID int64, address string) (bool, error) {
	res, err := q.ExecContext(ctx,
		`DELETE FROM workgroup_group_members WHERE group_id = $1 AND address = $2`,
		groupID, address)
	if err != nil {
		return false, fmt.Errorf("store: removing workgroup group member: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Expand returns the addresses of several groups, merged and deduplicated.
//
// It is the answer to "invite these groups": everything that names people in
// the end — an invitation, a share, an alias — wants addresses, so the groups
// are resolved here once rather than by each of those in its own way. The
// result is sorted so that the same groups always produce the same list, which
// matters when the list becomes an invitation that somebody might diff.
//
// An unknown group id is an error rather than a skip: silently dropping one
// means a meeting that quietly leaves people out.
func (g *Groups) Expand(ctx context.Context, q Querier, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[string]string, 32) // lowercased address -> address as stored
	for _, id := range ids {
		members, err := g.Members(ctx, q, id)
		if err != nil {
			return nil, err
		}
		for _, address := range members {
			key := strings.ToLower(address)
			if _, ok := seen[key]; !ok {
				seen[key] = address
			}
		}
	}
	out := make([]string, 0, len(seen))
	for _, address := range seen {
		out = append(out, address)
	}
	sort.Strings(out)
	return out, nil
}

// everyoneCount counts an organization's derived membership: the enabled
// mailboxes in the domains it owns.
//
// It goes through Workgroups.People rather than a count of its own so that the
// number shown beside the group and the addresses the group expands to are the
// same answer computed by the same code.
func (g *Groups) everyoneCount(ctx context.Context, q Querier, workgroupID int64) (int, error) {
	contacts, err := NewWorkgroups().People(ctx, q, workgroupID)
	if err != nil {
		return 0, err
	}
	return len(contacts), nil
}

// mapGroupErr turns a database error into the package's sentinels.
//
// A unique violation here is a name already used in this organization, and a
// foreign key violation is a group or an address that does not exist — both of
// which the caller can do something about, and neither of which is a fault.
func mapGroupErr(what string, err error) error {
	if IsNotFound(err) {
		return fmt.Errorf("store: %s: %w", what, ErrNotFound)
	}
	if IsConflict(err) {
		return fmt.Errorf("store: %s: %w", what, ErrConflict)
	}
	return fmt.Errorf("store: %s: %w", what, err)
}
