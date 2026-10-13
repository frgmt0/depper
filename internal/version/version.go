// Package version holds the depper build version.
package version

// Version is the depper release version. Release builds inject it with
// -ldflags "-X github.com/frgmt0/depper/internal/version.Version=v0.2.0".
var Version = "dev"
