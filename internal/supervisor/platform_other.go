//go:build !linux

package supervisor

import "errors"

// unsupportedPlatform lets the package build and unit-test on non-Linux dev machines; the
// real mounts/reboot only exist on Linux (platform_linux.go). Orchestration tests inject a
// fake Platform, so these stubs are never exercised in tests.
type unsupportedPlatform struct{}

func newPlatform() Platform { return unsupportedPlatform{} }

var errUnsupportedPlatform = errors.New("fc-supervisor platform operations require Linux")

func (unsupportedPlatform) MountBaseFilesystems() error      { return errUnsupportedPlatform }
func (unsupportedPlatform) MountWorkspace(_, _ string) error { return errUnsupportedPlatform }
func (unsupportedPlatform) UnmountWorkspace(_ string) error  { return errUnsupportedPlatform }
func (unsupportedPlatform) Sync()                            {}
func (unsupportedPlatform) PowerOff() error                  { return errUnsupportedPlatform }
