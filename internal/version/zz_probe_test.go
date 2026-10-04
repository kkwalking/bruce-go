package version

import "testing"

func TestProbeAlwaysFails(t *testing.T) {
	t.Fatal("probe: this failure exists only to test the branch ruleset")
}
