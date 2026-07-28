package fleetagent

import (
	"context"
	"strings"
	"sync"
)

// fakeRunner records commands instead of executing them, for unit tests.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	// err, if set, is returned for any command whose name+args joined by space contains
	// the key substring.
	errFor map[string]error
	out    map[string][]byte
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{errFor: map[string]error{}, out: map[string][]byte{}}
}

func (f *fakeRunner) record(name string, args []string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	return strings.Join(call, " ")
}

func (f *fakeRunner) matchErr(line string) error {
	for k, err := range f.errFor {
		if strings.Contains(line, k) {
			return err
		}
	}
	return nil
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	return f.matchErr(f.record(name, args))
}

func (f *fakeRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	line := f.record(name, args)
	if err := f.matchErr(line); err != nil {
		return nil, err
	}
	for k, v := range f.out {
		if strings.Contains(line, k) {
			return v, nil
		}
	}
	return nil, nil
}

func (f *fakeRunner) ran(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			return true
		}
	}
	return false
}
