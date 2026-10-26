module github.com/akira-toriyama/prq

// Floor is a supported minor (never an EOL pin); `toolchain` names the build
// toolchain. CI leaves GOTOOLCHAIN unset so setup-go reads this file, honors the
// `toolchain` line and pins it; a job-level GOTOOLCHAIN=local up front would make
// setup-go skip the toolchain line and install the bare floor instead. build.sh
// and scripts/check.sh leave it unset for the same reason; `go install @latest`
// is the one channel that builds with the local toolchain (README notes it).
go 1.26.0

toolchain go1.26.10

require (
	github.com/cli/go-gh/v2 v2.16.1
	github.com/cli/safeexec v1.0.1
)

require (
	github.com/aymanbagabas/go-osc52/v2 v2.0.1 // indirect
	github.com/cli/shurcooL-graphql v0.0.4 // indirect
	github.com/henvic/httpretty v0.2.0 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/lucasb-eyer/go-colorful v1.2.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/muesli/termenv v0.16.0 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/thlib/go-timezone-local v0.0.8 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/term v0.45.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
