// Package version holds build information.
package version

// Version is set at build time with -ldflags "-X github.com/ogefest/hoarfrost/internal/version.Version=...".
var Version = "dev"

// Software is the name used in WARC and WACZ metadata.
func Software() string { return "hoarfrost/" + Version }
