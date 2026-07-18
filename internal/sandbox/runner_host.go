package sandbox

import "context"

type hostRunner struct{}

func (hostRunner) Name() string { return "none" }

func (hostRunner) Probe(context.Context) Capabilities {
	return Capabilities{Backend: "none", Available: true}
}

func (hostRunner) Run(ctx context.Context, spec CommandSpec, _ Policy) (RunResult, error) {
	return runProcess(ctx, "/bin/bash", []string{"-lc", spec.Command}, spec)
}

func (hostRunner) PrepareProcess(spec ProcessSpec, _ Policy) (PreparedProcess, error) {
	if spec.Program == "" {
		return PreparedProcess{}, ErrPolicy
	}
	return PreparedProcess{
		Program:     spec.Program,
		Args:        append([]string(nil), spec.Args...),
		Directory:   spec.Directory,
		Environment: append([]string(nil), spec.Environment...),
	}, nil
}
