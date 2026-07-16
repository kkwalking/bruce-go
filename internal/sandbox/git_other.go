//go:build !darwin && !linux

package sandbox

func discoverGitLayout(string) (GitLayout, error) { return GitLayout{}, nil }
