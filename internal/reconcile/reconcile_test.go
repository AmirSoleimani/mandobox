package reconcile

import (
	"testing"
	"time"

	"github.com/chelodo/mandobox/internal/session"
)

func TestFindOrphans(t *testing.T) {
	live, _ := session.New()
	orphan, _ := session.New()
	young, _ := session.New()
	now := time.Unix(10_000, 0)
	grace := 3 * time.Minute

	actual := []VM{
		{Session: live, StartedAt: 10_000 - 600},   // 10m old, expected → keep
		{Session: orphan, StartedAt: 10_000 - 600}, // 10m old, not expected → reap
		{Session: young, StartedAt: 10_000 - 60},   // 1m old, not expected but within grace → keep
	}
	expected := map[session.ID]struct{}{live: {}}

	got := FindOrphans(actual, expected, grace, now)
	if len(got) != 1 || got[0] != orphan {
		t.Fatalf("expected only %s reaped, got %v", orphan, got)
	}
}

func TestFindOrphansNothingWhenAllExpected(t *testing.T) {
	a, _ := session.New()
	b, _ := session.New()
	now := time.Unix(10_000, 0)
	actual := []VM{{Session: a, StartedAt: 0}, {Session: b, StartedAt: 0}}
	expected := map[session.ID]struct{}{a: {}, b: {}}
	if got := FindOrphans(actual, expected, time.Minute, now); len(got) != 0 {
		t.Fatalf("expected no orphans when all VMs are claimed, got %v", got)
	}
}
