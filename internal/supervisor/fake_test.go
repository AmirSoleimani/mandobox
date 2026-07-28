package supervisor

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
)

// fakeRunner records commands and returns canned output/errors matched by substring.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []string
	outputs map[string]string
	errs    map[string]error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{outputs: map[string]string{}, errs: map[string]error{}}
}

func (f *fakeRunner) line(name string, args []string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, l)
	return l
}

func (f *fakeRunner) errFor(line string) error {
	for k, e := range f.errs {
		if strings.Contains(line, k) {
			return e
		}
	}
	return nil
}

func (f *fakeRunner) outFor(line string) string {
	for k, v := range f.outputs {
		if strings.Contains(line, k) {
			return v
		}
	}
	return ""
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	return f.errFor(f.line(name, args))
}

func (f *fakeRunner) RunEnv(_ context.Context, _ []string, name string, args ...string) error {
	return f.errFor(f.line(name, args))
}

func (f *fakeRunner) Output(_ context.Context, name string, args ...string) (string, error) {
	l := f.line(name, args)
	return f.outFor(l), f.errFor(l)
}

func (f *fakeRunner) OutputEnv(_ context.Context, _ []string, name string, args ...string) (string, error) {
	l := f.line(name, args)
	return f.outFor(l), f.errFor(l)
}

func (f *fakeRunner) ran(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// fakeAgent returns a canned Result and records the spec it was given.
type fakeAgent struct {
	result  Result
	err     error
	gotSpec AgentSpec
}

func (a *fakeAgent) Run(_ context.Context, spec AgentSpec, onLine func([]byte)) (Result, error) {
	a.gotSpec = spec
	if onLine != nil {
		onLine([]byte(`{"type":"assistant"}`))
	}
	return a.result, a.err
}

// fakeTransport captures published messages and the command handler.
type fakeTransport struct {
	mu         sync.Mutex
	published  map[string][][]byte
	cmdHandler func([]byte)
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{published: map[string][][]byte{}}
}

func (t *fakeTransport) Publish(subject string, data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	cp := append([]byte(nil), data...)
	t.published[subject] = append(t.published[subject], cp)
	return nil
}

func (t *fakeTransport) Subscribe(_ string, handler func([]byte)) error {
	t.cmdHandler = handler
	return nil
}

func (t *fakeTransport) Flush() error { return nil }
func (t *fakeTransport) Close() error { return nil }

// events decodes all Events published to any *.event subject.
func (t *fakeTransport) events() []Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Event
	for subj, msgs := range t.published {
		if !strings.HasSuffix(subj, ".event") {
			continue
		}
		for _, m := range msgs {
			var e Event
			if json.Unmarshal(m, &e) == nil {
				out = append(out, e)
			}
		}
	}
	return out
}

// fakePlatform satisfies Platform without touching the OS.
type fakePlatform struct{ poweredOff bool }

func (p *fakePlatform) MountBaseFilesystems() error      { return nil }
func (p *fakePlatform) MountWorkspace(_, _ string) error { return nil }
func (p *fakePlatform) UnmountWorkspace(_ string) error  { return nil }
func (p *fakePlatform) Sync()                            {}
func (p *fakePlatform) PowerOff() error                  { p.poweredOff = true; return nil }
