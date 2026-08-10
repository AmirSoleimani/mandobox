package supervisor

import (
	"strings"
	"testing"
)

func TestPreambleOverride(t *testing.T) {
	// No override → built-in defaults.
	s := &Supervisor{cfg: BootConfig{}}
	if s.autonomousPreambleText() != autonomousPreamble {
		t.Error("autonomous: expected built-in default when no override")
	}
	if s.collaboratePreambleText() != collaboratePreamble {
		t.Error("collaborate: expected built-in default when no override")
	}

	// Override set → used, with a trailing separator so the task doesn't run into it.
	s = &Supervisor{cfg: BootConfig{Agent: AgentConfig{
		PreambleAutonomous:  "Be brief.",
		PreambleCollaborate: "Be kind.",
	}}}
	if got := s.autonomousPreambleText(); got != "Be brief.\n\n" {
		t.Errorf("autonomous override = %q", got)
	}
	if got := s.collaboratePreambleText(); !strings.HasPrefix(got, "Be kind.") {
		t.Errorf("collaborate override = %q", got)
	}

	// Whitespace-only override is treated as empty → default.
	s = &Supervisor{cfg: BootConfig{Agent: AgentConfig{PreambleAutonomous: "   \n"}}}
	if s.autonomousPreambleText() != autonomousPreamble {
		t.Error("whitespace-only override should fall back to default")
	}
}

func TestDefaultPreamblesExported(t *testing.T) {
	if DefaultAutonomousPreamble != autonomousPreamble || DefaultCollaboratePreamble != collaboratePreamble {
		t.Fatal("exported default preambles must match the internal constants")
	}
}
