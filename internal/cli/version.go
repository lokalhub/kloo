package cli

import "fmt"

// Build-stamp variables. Overridden at release time by goreleaser via ldflags
// (-X github.com/lokalhub/kloo/internal/cli.version=… etc.); the defaults below are
// what a plain `go build` / `go install` produces.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// versionString renders the `kloo --version` line: "kloo <version> (<commit>, <date>)".
func versionString() string {
	return fmt.Sprintf("%s (%s, %s)", version, commit, date)
}

// Version returns the bare build version ("dev" for a plain go build, the
// semver for a goreleaser build). Exported so the TUI can show it in the header.
func Version() string { return version }
