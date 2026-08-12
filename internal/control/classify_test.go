package control

import (
	"context"
	"testing"
)

// The plan-decision mapping is the safety-critical bit: only an explicit PROCEED starts a build; every
// other reply (and anything unrecognized) must stay in discussion.
func TestPlanDecisionFromText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"PROCEED", "proceed"},
		{"proceed", "proceed"},
		{"Proceed.", "proceed"},
		{"PROCEED — go build it", "proceed"},
		{"proceeding", "proceed"},
		{"DISCUSS", "discuss"},
		{"do not proceed yet", "discuss"}, // negation must not fail open
		{"let's change the auth part first", "discuss"},
		{"what about error handling?", "discuss"},
		{"maybe", "discuss"},
		{"", "discuss"},
	}
	for _, c := range cases {
		if got := planDecisionFromText(c.in); got != c.want {
			t.Errorf("planDecisionFromText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// An empty reply fails safe to discuss without any network call.
func TestClassifyPlanDecisionEmptyFailsSafe(t *testing.T) {
	got, err := (&Activities{}).ClassifyPlanDecision(context.Background(), "   ")
	if err != nil || got != "discuss" {
		t.Fatalf("empty message = %q, %v; want discuss, nil", got, err)
	}
}
