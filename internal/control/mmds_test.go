package control

import (
	"testing"

	"github.com/AmirSoleimani/mandobox/internal/supervisor"
)

// buildMMDS carries the task prompt and/or reviewer instructions to the guest per launch mode:
// initial → prompt; resume → instructions; plan/execute → prompt + instructions (when any).
func TestBuildMMDSTaskByMode(t *testing.T) {
	base := LaunchParams{
		Input:        WorkflowInput{Prompt: "add a /healthz endpoint", Repo: "o/r", CloneURL: "u", BaseBranch: "main"},
		Instructions: []string{"use net/http"},
	}
	cases := []struct {
		mode                     string
		wantPrompt, wantInstruct bool
	}{
		{supervisor.ModeInitial, true, false},
		{supervisor.ModeResume, false, true},
		{supervisor.ModePlan, true, true},
		{supervisor.ModeExecute, true, true},
	}
	for _, c := range cases {
		p := base
		p.Mode = c.mode
		task := buildMMDS(p)["task"].(map[string]any)
		if task["mode"] != c.mode {
			t.Errorf("%s: mode = %v", c.mode, task["mode"])
		}
		if _, ok := task["prompt"]; ok != c.wantPrompt {
			t.Errorf("%s: prompt present = %v, want %v", c.mode, ok, c.wantPrompt)
		}
		if _, ok := task["instructions"]; ok != c.wantInstruct {
			t.Errorf("%s: instructions present = %v, want %v", c.mode, ok, c.wantInstruct)
		}
	}

	// Plan/execute with no reviewer feedback: prompt present, instructions omitted (not an empty list).
	p := base
	p.Mode = supervisor.ModePlan
	p.Instructions = nil
	task := buildMMDS(p)["task"].(map[string]any)
	if _, ok := task["instructions"]; ok {
		t.Errorf("plan with no feedback should omit instructions, got %v", task["instructions"])
	}
	if task["prompt"] != "add a /healthz endpoint" {
		t.Errorf("plan prompt = %v", task["prompt"])
	}
}
