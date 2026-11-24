package task

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	fbauth "firebase.google.com/go/v4/auth"
)

// fakeUsers answers GetUsers from a set of accounts, recording batch sizes.
type fakeUsers struct {
	users   map[string]*fbauth.UserRecord
	batches []int
	err     error
}

func (f *fakeUsers) GetUsers(_ context.Context, ids []fbauth.UserIdentifier) (*fbauth.GetUsersResult, error) {
	f.batches = append(f.batches, len(ids))
	if f.err != nil {
		return nil, f.err
	}
	result := &fbauth.GetUsersResult{}
	for _, id := range ids {
		uid := id.(fbauth.UIDIdentifier).UID
		if u, ok := f.users[uid]; ok {
			result.Users = append(result.Users, u)
		} else {
			result.NotFound = append(result.NotFound, id)
		}
	}
	return result, nil
}

func record(uid string, disabled bool, created, login, refresh time.Time) *fbauth.UserRecord {
	ms := func(t time.Time) int64 {
		if t.IsZero() {
			return 0
		}
		return t.UnixMilli()
	}
	return &fbauth.UserRecord{UserInfo: &fbauth.UserInfo{UID: uid}, Disabled: disabled,
		UserMetadata: &fbauth.UserMetadata{CreationTimestamp: ms(created), LastLogInTimestamp: ms(login), LastRefreshTimestamp: ms(refresh)}}
}

func TestFirebaseAccountsLooksUpOwnersInBatches(t *testing.T) {
	f := &fakeUsers{users: map[string]*fbauth.UserRecord{}}
	var ids []string
	for i := range 230 {
		id := fmt.Sprintf("user-%03d", i)
		ids = append(ids, id)
		if i%2 == 0 {
			f.users[id] = record(id, false, now.Add(-time.Hour), time.Time{}, time.Time{})
		}
	}
	accounts, err := FirebaseAccounts(f).LookupAccounts(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(f.batches) != "[100 100 30]" {
		t.Fatalf("batches %v, want at most 100 IDs per call", f.batches)
	}
	if len(accounts) != 230 || !accounts["user-000"].Exists || accounts["user-001"].Exists {
		t.Fatalf("got %d accounts, user-000 %+v, user-001 %+v", len(accounts), accounts["user-000"], accounts["user-001"])
	}
}

func TestFirebaseAccountsReadsStateAndLastActivity(t *testing.T) {
	created, login, refresh := now.Add(-300*24*time.Hour), now.Add(-100*24*time.Hour), now.Add(-2*24*time.Hour)
	f := &fakeUsers{users: map[string]*fbauth.UserRecord{
		"refreshed": record("refreshed", false, created, login, refresh),
		"disabled":  record("disabled", true, created, login, time.Time{}),
	}}
	tooLong := strings.Repeat("x", maxFirebaseUIDLength+1)
	accounts, err := FirebaseAccounts(f).LookupAccounts(context.Background(), []string{"refreshed", "disabled", "", tooLong, "deleted"})
	if err != nil {
		t.Fatal(err)
	}
	if a := accounts["refreshed"]; !a.Exists || a.Disabled || !a.LastActive.Equal(refresh.Truncate(time.Millisecond)) {
		t.Errorf("refreshed: %+v, want present with the refresh as last activity", a)
	}
	if a := accounts["disabled"]; !a.Exists || !a.Disabled || !a.LastActive.Equal(login.Truncate(time.Millisecond)) {
		t.Errorf("disabled: %+v", a)
	}
	for _, id := range []string{"", tooLong, "deleted"} {
		if a, ok := accounts[id]; !ok || a.Exists {
			t.Errorf("%.10q: %+v (%v), want settled as missing", id, a, ok)
		}
	}
	if fmt.Sprint(f.batches) != "[3]" {
		t.Errorf("batches %v: IDs Firebase cannot hold must not be sent", f.batches)
	}
}

func TestFirebaseAccountsFailsAsAWhole(t *testing.T) {
	f := &fakeUsers{err: errors.New("unavailable")}
	if accounts, err := FirebaseAccounts(f).LookupAccounts(context.Background(), []string{"a"}); err == nil || accounts != nil {
		t.Fatalf("accounts %v, err %v; want an error and no accounts", accounts, err)
	}
}
