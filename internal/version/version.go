// Package version carries build identity so every binary can say exactly what
// it is. GoReleaser (M6) injects these with -ldflags -X.
package version

import "fmt"

// Injected at build time; the defaults describe a plain `go build`.
var (
	Version = "0.2.0"
	Commit  = "dev"
	Date    = "unknown"
)

// String renders the full identity, e.g. "0.1.0 (abc1234, 2026-11-07)".
func String() string {
	if Commit == "dev" {
		return Version + " (dev build)"
	}
	return fmt.Sprintf("%s (%s, %s)", Version, Commit, Date)
}
