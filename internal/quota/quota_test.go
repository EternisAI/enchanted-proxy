package quota

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

type fakeMembers struct {
	groups map[string][]string // user ID -> members of its group
	calls  int
	err    error
}

func (f *fakeMembers) ListQuotaGroupMembers(_ context.Context, userID string) ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return slices.Clone(f.groups[userID]), nil
}

func TestUserIDsReturnsTheWholeGroupWithTheCaller(t *testing.T) {
	members := &fakeMembers{groups: map[string][]string{"b": {"c", "b", "a"}}}
	g := NewGroups(members)

	got, err := g.UserIDs(context.Background(), "b")
	if err != nil || !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("group of b = %v, %v; want [a b c]", got, err)
	}
	got, err = g.UserIDs(context.Background(), "solo")
	if err != nil || !slices.Equal(got, []string{"solo"}) {
		t.Fatalf("group of an account in no group = %v, %v; want [solo]", got, err)
	}
}

func TestUserIDsWithoutAStoreIsAGroupOfOne(t *testing.T) {
	got, err := NewGroups(nil).UserIDs(context.Background(), "a")
	if err != nil || !slices.Equal(got, []string{"a"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

// A failed lookup must not shrink the group to the caller alone: limits would
// then count less usage than the group's.
func TestUserIDsFailsWhenTheLookupFails(t *testing.T) {
	members := &fakeMembers{err: errors.New("db down")}
	g := NewGroups(members)
	if got, err := g.UserIDs(context.Background(), "a"); err == nil {
		t.Fatalf("got %v and no error", got)
	}
	members.err = nil
	members.groups = map[string][]string{"a": {"a", "b"}}
	if got, err := g.UserIDs(context.Background(), "a"); err != nil || len(got) != 2 {
		t.Fatalf("after recovery got %v, %v; a failure must not be cached", got, err)
	}
}

func TestUserIDsCachesForAMinute(t *testing.T) {
	members := &fakeMembers{groups: map[string][]string{"a": {"a", "b"}}}
	g := NewGroups(members)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }

	for range 3 {
		if _, err := g.UserIDs(context.Background(), "a"); err != nil {
			t.Fatal(err)
		}
	}
	if members.calls != 1 {
		t.Fatalf("lookups = %d within the cache lifetime, want 1", members.calls)
	}

	members.groups["a"] = []string{"a", "b", "c"}
	now = now.Add(cacheTTL)
	got, _ := g.UserIDs(context.Background(), "a")
	if members.calls != 2 || len(got) != 3 {
		t.Fatalf("after the cache lifetime: lookups %d, group %v; want a fresh read", members.calls, got)
	}
}

func TestChangedDropsEveryCachedGroup(t *testing.T) {
	members := &fakeMembers{groups: map[string][]string{"a": {"a"}, "c": {"c"}}}
	g := NewGroups(members)
	_, _ = g.UserIDs(context.Background(), "a")
	_, _ = g.UserIDs(context.Background(), "c")

	// A join elsewhere put a, b and c together.
	members.groups["a"] = []string{"a", "b", "c"}
	members.groups["c"] = []string{"a", "b", "c"}
	g.Changed()
	for _, id := range []string{"a", "c"} {
		if got, _ := g.UserIDs(context.Background(), id); len(got) != 3 {
			t.Fatalf("group of %s after Changed = %v, want the joined group", id, got)
		}
	}
}

func TestReturnedGroupsDoNotShareTheCache(t *testing.T) {
	g := NewGroups(&fakeMembers{groups: map[string][]string{"a": {"a", "b"}}})
	got, _ := g.UserIDs(context.Background(), "a")
	got[0] = "changed"
	if again, _ := g.UserIDs(context.Background(), "a"); again[0] != "a" {
		t.Fatalf("a caller's change reached the cache: %v", again)
	}
}
