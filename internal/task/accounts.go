package task

import (
	"context"
	"fmt"
	"time"

	fbauth "firebase.google.com/go/v4/auth"
)

// Account is what the reconciler needs to know about a task's owner.
type Account struct {
	Exists   bool
	Disabled bool
	// LastActive is the latest of the account's creation, last sign-in and last
	// token refresh. Zero when Firebase reported none of them.
	LastActive time.Time
	// Created is when the account was created. Zero when Firebase did not say.
	Created time.Time
}

// AccountLookup looks up task owners. An ID with no entry in the result could
// not be settled; an error means nothing is known about any of them.
type AccountLookup interface {
	LookupAccounts(ctx context.Context, userIDs []string) (map[string]Account, error)
}

// firebaseUsers is the part of the Firebase Auth client the lookup uses.
type firebaseUsers interface {
	GetUsers(ctx context.Context, identifiers []fbauth.UserIdentifier) (*fbauth.GetUsersResult, error)
}

// firebaseUsersBatch is the most accounts Firebase Auth returns in one call.
const firebaseUsersBatch = 100

// maxFirebaseUIDLength is the longest user ID Firebase Auth accepts. An ID it
// cannot hold names no account.
const maxFirebaseUIDLength = 128

// FirebaseAccounts looks task owners up in Firebase Auth.
func FirebaseAccounts(client firebaseUsers) AccountLookup {
	return firebaseAccounts{client: client}
}

type firebaseAccounts struct{ client firebaseUsers }

func (f firebaseAccounts) LookupAccounts(ctx context.Context, userIDs []string) (map[string]Account, error) {
	accounts := make(map[string]Account, len(userIDs))
	var batch []fbauth.UserIdentifier
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		result, err := f.client.GetUsers(ctx, batch)
		if err != nil {
			return fmt.Errorf("failed to look up accounts: %w", err)
		}
		for _, u := range result.Users {
			if u == nil || u.UserInfo == nil {
				continue
			}
			accounts[u.UID] = Account{Exists: true, Disabled: u.Disabled, LastActive: lastActive(u.UserMetadata), Created: created(u.UserMetadata)}
		}
		for _, id := range result.NotFound {
			if uid, ok := id.(fbauth.UIDIdentifier); ok {
				accounts[uid.UID] = Account{}
			}
		}
		batch = batch[:0]
		return nil
	}
	for _, id := range userIDs {
		if id == "" || len(id) > maxFirebaseUIDLength {
			accounts[id] = Account{} // Firebase cannot hold such an ID
			continue
		}
		batch = append(batch, fbauth.UIDIdentifier{UID: id})
		if len(batch) == firebaseUsersBatch {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return accounts, nil
}

func created(m *fbauth.UserMetadata) time.Time {
	if m == nil || m.CreationTimestamp <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(m.CreationTimestamp).UTC()
}

func lastActive(m *fbauth.UserMetadata) time.Time {
	if m == nil {
		return time.Time{}
	}
	latest := max(m.CreationTimestamp, m.LastLogInTimestamp, m.LastRefreshTimestamp)
	if latest <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(latest).UTC()
}
