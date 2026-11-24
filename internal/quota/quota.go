// Package quota decides which accounts' usage counts together.
//
// Every limit is counted over a quota group: all accounts that share a
// purchase. A purchase that reaches a second account joins the two accounts'
// groups (Join). An account no purchase has joined to another is a group of one.
package quota

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
)

// Members reads quota group members from the database.
type Members interface {
	ListQuotaGroupMembers(ctx context.Context, userID string) ([]string, error)
}

// cacheTTL is how long a group read from the database is used. A join made by
// another server instance applies here within this time.
const cacheTTL = time.Minute

// maxCached bounds the cache; past it, expired entries are dropped, and the
// whole cache if that is not enough.
const maxCached = 10_000

// Groups returns the members of an account's quota group.
type Groups struct {
	members Members
	ttl     time.Duration
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]cachedGroup
}

type cachedGroup struct {
	userIDs []string
	until   time.Time
}

// NewGroups returns the group lookup. With no members store, every account is
// a group of one.
func NewGroups(members Members) *Groups {
	return &Groups{members: members, ttl: cacheTTL, now: time.Now, cache: map[string]cachedGroup{}}
}

// UserIDs returns the user IDs whose usage counts toward userID's limits,
// userID included, sorted. Limits query with `user_id = ANY(...)` over this
// list. A failed lookup returns an error, never a smaller group.
func (g *Groups) UserIDs(ctx context.Context, userID string) ([]string, error) {
	if g == nil || g.members == nil {
		return []string{userID}, nil
	}
	now := g.now()
	g.mu.Lock()
	c, ok := g.cache[userID]
	g.mu.Unlock()
	if ok && now.Before(c.until) {
		return slices.Clone(c.userIDs), nil
	}

	members, err := g.members.ListQuotaGroupMembers(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to look up quota group: %w", err)
	}
	userIDs := slices.Clone(members)
	if !slices.Contains(userIDs, userID) {
		userIDs = append(userIDs, userID)
	}
	slices.Sort(userIDs)

	g.mu.Lock()
	if len(g.cache) >= maxCached {
		for id, e := range g.cache {
			if !now.Before(e.until) {
				delete(g.cache, id)
			}
		}
		if len(g.cache) >= maxCached {
			clear(g.cache)
		}
	}
	g.cache[userID] = cachedGroup{userIDs: userIDs, until: now.Add(g.ttl)}
	g.mu.Unlock()
	return slices.Clone(userIDs), nil
}

// Changed drops every cached group, so a join made by this server instance
// applies to its next lookups at once. A join changes the group of every
// member of both groups, not only of the two accounts named.
func (g *Groups) Changed() {
	if g == nil {
		return
	}
	g.mu.Lock()
	clear(g.cache)
	g.mu.Unlock()
}

// Store is what Join needs, through a querier bound to a transaction.
type Store interface {
	LockQuotaGroups(ctx context.Context) error
	GetQuotaGroupID(ctx context.Context, userID string) (string, error)
	SetQuotaGroup(ctx context.Context, arg pgdb.SetQuotaGroupParams) error
	MoveQuotaGroupMembers(ctx context.Context, arg pgdb.MoveQuotaGroupMembersParams) error
}

// Join puts two accounts, and everyone already in a group with either of
// them, into one group: a's group, which b's group's members move into. It
// reports whether anything changed. It must run inside a transaction, as it
// takes the quota group lock until the transaction ends.
func Join(ctx context.Context, q Store, a, b string) (bool, error) {
	if a == b {
		return false, nil
	}
	if err := q.LockQuotaGroups(ctx); err != nil {
		return false, fmt.Errorf("failed to lock quota groups: %w", err)
	}
	into, err := q.GetQuotaGroupID(ctx, a)
	if err != nil {
		return false, fmt.Errorf("failed to read quota group: %w", err)
	}
	from, err := q.GetQuotaGroupID(ctx, b)
	if err != nil {
		return false, fmt.Errorf("failed to read quota group: %w", err)
	}
	if into == from {
		return false, nil
	}
	// a may be in no group yet, which makes its own ID its group.
	if err := q.SetQuotaGroup(ctx, pgdb.SetQuotaGroupParams{UserID: a, GroupID: into}); err != nil {
		return false, fmt.Errorf("failed to join quota groups: %w", err)
	}
	if err := q.MoveQuotaGroupMembers(ctx, pgdb.MoveQuotaGroupMembersParams{IntoGroupID: into, FromGroupID: from}); err != nil {
		return false, fmt.Errorf("failed to join quota groups: %w", err)
	}
	// b may be in no group yet, so the move did not reach it.
	if err := q.SetQuotaGroup(ctx, pgdb.SetQuotaGroupParams{UserID: b, GroupID: into}); err != nil {
		return false, fmt.Errorf("failed to join quota groups: %w", err)
	}
	return true, nil
}
