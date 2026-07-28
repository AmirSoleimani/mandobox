package fleetagent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// Runner runs external commands (ip, cp, mkfs, jailer). It is an interface so the VM
// lifecycle can be unit-tested with a fake that records calls instead of touching the host.
type Runner interface {
	// Run executes name with args and returns an error including captured stderr on failure.
	Run(ctx context.Context, name string, args ...string) error
	// Output executes name with args and returns stdout.
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

// execRunner is the production Runner backed by os/exec.
type execRunner struct{}

// NewExecRunner returns a Runner that shells out to real commands.
func NewExecRunner() Runner { return execRunner{} }

func (execRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, stderr.String())
	}
	return nil
}

func (execRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %v: %w: %s", name, args, err, stderr.String())
	}
	return stdout.Bytes(), nil
}
