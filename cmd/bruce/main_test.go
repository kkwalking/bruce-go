package main

import "testing"

func TestParseResumeCommand(t *testing.T) {
	for _, tt := range []struct {
		args   []string
		resume bool
		ref    string
		bad    bool
	}{
		{},
		{args: []string{"resume"}, resume: true},
		{args: []string{"resume", "/path with space/session.jsonl"}, resume: true, ref: "/path with space/session.jsonl"},
		{args: []string{"unknown"}, bad: true},
		{args: []string{"resume", "one", "two"}, bad: true},
	} {
		resume, ref, err := parseCommandArgs(tt.args)
		if resume != tt.resume || ref != tt.ref || (err != nil) != tt.bad {
			t.Fatalf("args=%v: %v %q %v", tt.args, resume, ref, err)
		}
	}
}
