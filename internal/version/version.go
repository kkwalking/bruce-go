// Package version provides the application version shared by all interfaces.
package version

// Current is the application version reported by all interfaces.
//
// It is a var, not a const, so the build can set it:
//
//	go build -ldflags "-X bruce-go/internal/version.Current=0.9.0"
//
// `make build` derives that value from git describe, so the git tag is the
// single source of truth and this default is only what a bare
// `go build ./cmd/bruce` reports. See AGENTS.md「版本与发布」.
var Current = "dev"
