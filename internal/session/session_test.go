package session

import "testing"

func TestParse(t *testing.T) {
	valid := []string{
		"s_0123456789ABCDEFGHJKMNPQRS",
		"s_ZZZZZZZZZZZZZZZZZZZZZZZZZZ",
		"s_00000000000000000000000000",
	}
	for _, s := range valid {
		if _, err := Parse(s); err != nil {
			t.Errorf("Parse(%q) unexpected error: %v", s, err)
		}
	}

	invalid := []string{
		"",
		"s_",
		"0123456789ABCDEFGHJKMNPQRS",    // missing prefix
		"s_0123456789ABCDEFGHJKMNPQR",   // 25 chars
		"s_0123456789ABCDEFGHJKMNPQRST", // 27 chars
		"s_0123456789ABCDEFGHJKMNPQRI",  // I not allowed
		"s_0123456789ABCDEFGHJKMNPQRL",  // L not allowed
		"s_0123456789ABCDEFGHJKMNPQRO",  // O not allowed
		"s_0123456789ABCDEFGHJKMNPQRU",  // U not allowed
		"s_0123456789abcdefghjkmnpqrs",  // lowercase
		"X_0123456789ABCDEFGHJKMNPQRS",  // wrong prefix
	}
	for _, s := range invalid {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) expected error, got nil", s)
		}
	}
}

func TestNewIsValidAndUnique(t *testing.T) {
	seen := make(map[ID]struct{})
	for range 1000 {
		id, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if !id.Valid() {
			t.Fatalf("New produced invalid id: %q", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("New produced a duplicate: %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestDerivations(t *testing.T) {
	id := MustParse("s_0123456789ABCDEFGHJKMNPQRS")

	if got, want := id.Branch(), "agent/s_0123456789ABCDEFGHJKMNPQRS"; got != want {
		t.Errorf("Branch() = %q, want %q", got, want)
	}
	if got, want := id.SubjectPrefix(), "agent.s_0123456789ABCDEFGHJKMNPQRS"; got != want {
		t.Errorf("SubjectPrefix() = %q, want %q", got, want)
	}
	if got, want := id.WorkflowID(), "s_0123456789ABCDEFGHJKMNPQRS"; got != want {
		t.Errorf("WorkflowID() = %q, want %q", got, want)
	}
}

func TestTapNameFitsIfnamsiz(t *testing.T) {
	for range 100 {
		id, err := New()
		if err != nil {
			t.Fatal(err)
		}
		tap := id.TapName()
		if len(tap) > 15 {
			t.Fatalf("TapName() = %q is %d chars, exceeds IFNAMSIZ-1 (15)", tap, len(tap))
		}
		if tap[:3] != "tap" {
			t.Fatalf("TapName() = %q must start with tap", tap)
		}
	}
}
