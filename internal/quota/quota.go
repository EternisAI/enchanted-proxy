// Package quota decides which accounts' usage counts together.
//
// Every limit is counted over a quota group: all accounts that share a
// purchase. Until purchases are bound to accounts, an account is a group of one.
package quota

import "context"

// Groups returns the members of an account's quota group.
type Groups struct{}

// NewGroups returns the group lookup.
func NewGroups() *Groups {
	return &Groups{}
}

// UserIDs returns the user IDs whose usage counts toward userID's limits,
// userID included. Limits query with `user_id = ANY(...)` over this list.
func (g *Groups) UserIDs(_ context.Context, userID string) ([]string, error) {
	return []string{userID}, nil
}
