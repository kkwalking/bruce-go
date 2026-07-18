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
	return Capabilities{Backend: "unsupported", Reason: "当前平台暂不支持原生 sandbox: " + runtime.GOOS}
}

func (unsupportedRunner) Run(context.Context, CommandSpec, Policy) (RunResult, error) {
	return RunResult{}, ErrUnavailable
}

func (unsupportedRunner) PrepareProcess(ProcessSpec, Policy) (PreparedProcess, error) {
	return PreparedProcess{}, ErrUnavailable
}
