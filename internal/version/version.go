// Package version isolates build identity, overridden at release time via
// -ldflags "-X github.com/akira-toriyama/prq/internal/version.<Field>=...".
// Defaults describe a from-source build: Version "dev", with Commit/Date left
// for the runtime/debug VCS stamp to fill in — an un-stamped `go build` binary
// is still identifiable.
package version

import "runtime/debug"

// Build identity, injected by the linker at release/install time. Do not set
// these here: "dev" with empty Commit/Date is the only shape a bare `go build`
// starts from — Resolve then backfills Commit/Date from the Go VCS stamp.
var (
	// Version is the semver tag of the build; "dev" from source.
	Version = "dev"
	// Commit is the full VCS revision the binary was built from.
	Commit = ""
	// Date is the commit/build timestamp (RFC 3339).
	Date = ""
)

// Info is the resolved build identity: the ldflags values, with Commit/Date
// backfilled from the Go build's VCS stamp when they were not injected.
type Info struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	Date     string `json:"date"`
	Modified bool   `json:"modified"`
}

// Resolve reads the injected build vars and, when Commit or Date is missing,
// falls back to runtime/debug.ReadBuildInfo (vcs.revision/vcs.time/
// vcs.modified). It never panics and is safe to call repeatedly.
func Resolve() Info {
	info := Info{Version: Version, Commit: Commit, Date: Date}
	if info.Commit != "" && info.Date != "" {
		return info
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if info.Commit == "" {
				info.Commit = s.Value
			}
		case "vcs.time":
			if info.Date == "" {
				info.Date = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				info.Modified = true
			}
		}
	}
	return info
}

// String renders the identity as one human line, e.g.
// "dev (abc1234, 2026-07-10T00:00:00Z)".
func (i Info) String() string {
	s := i.Version
	if i.Commit == "" {
		return s
	}
	short := i.Commit
	if len(short) > 7 {
		short = short[:7]
	}
	s += " (" + short
	if i.Date != "" {
		s += ", " + i.Date
	}
	if i.Modified {
		s += ", modified"
	}
	return s + ")"
}
