//go:build !darwin && !linux

package sandbox

import (
	"context"
	"runtime"
)

type unsupportedRunner struct{}

func newPlatformRunner(string) Runner { return unsupportedRunner{} }

func (unsupportedRunner) Name() string { return "unsupported" }

func (unsupportedRunner) Probe(context.Context) Capabilities {
	return Capabilities{Backend: "unsupported", Reason: "native sandboxing is not currently supported on this platform: " + runtime.GOOS}
}

func (unsupportedRunner) Run(context.Context, CommandSpec, Policy) (RunResult, error) {
	return RunResult{}, ErrUnavailable
}

func (unsupportedRunner) PrepareProcess(ProcessSpec, Policy) (PreparedProcess, error) {
	return PreparedProcess{}, ErrUnavailable
}
