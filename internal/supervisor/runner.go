package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
)

// Runner runs external commands (ip, git, gh). It is an interface so the supervisor's git
// and network logic can be unit-tested with a fake.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) error
	Output(ctx context.Context, name string, args ...string) (string, error)
	// RunEnv runs with extra environment entries appended (e.g. GH_TOKEN scoped to a single
	// gh invocation, never the global/agent environment).
	RunEnv(ctx context.Context, env []string, name string, args ...string) error
	// OutputEnv captures stdout with extra environment entries appended.
	OutputEnv(ctx context.Context, env []string, name string, args ...string) (string, error)
}

// execRunner is the production Runner.
type execRunner struct{}

// NewExecRunner returns a Runner backed by os/exec.
func NewExecRunner() Runner { return execRunner{} }

func (execRunner) Run(ctx context.Context, name string, args ...string) error {
	return execRunner{}.RunEnv(ctx, nil, name, args...)
}

func (execRunner) RunEnv(ctx context.Context, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, stderr.String())
	}
	return nil
}

func (execRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	return execRunner{}.OutputEnv(ctx, nil, name, args...)
}

func (execRunner) OutputEnv(ctx context.Context, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %v: %w: %s", name, args, err, stderr.String())
	}
	return stdout.String(), nil
}
