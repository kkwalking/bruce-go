package version

import (
	"regexp"
	"strings"
	"testing"
)

func TestCurrentIsNonEmpty(t *testing.T) {
	if Current == "" {
		t.Fatal("Current must not be empty")
	}
}

func TestCurrentMatchesSemanticVersion(t *testing.T) {
	semver := regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	if !semver.MatchString(Current) {
		t.Fatalf("Current = %q, want MAJOR.MINOR.PATCH", Current)
	}
}

func TestCurrentHasNoSurroundingWhitespace(t *testing.T) {
	if Current != strings.TrimSpace(Current) {
		t.Fatalf("Current = %q, must not have leading or trailing whitespace", Current)
	}
}
