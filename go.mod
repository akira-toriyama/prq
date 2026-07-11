module github.com/akira-toriyama/prq

// Floor pinned to a patched 1.25.x: go-version-file drives CI/release to build
// with exactly this toolchain, so the shipped binary carries current stdlib
// security fixes (crypto/tls GO-2026-5856 is fixed here). 1.23 was EOL. The
// 1.25 floor also lets go-gh v2 reach v2.13.0 (which requires go 1.25.0).
go 1.25.12

require (
	github.com/cli/go-gh/v2 v2.13.0
	github.com/cli/safeexec v1.0.0
)

require (
	github.com/aymanbagabas/go-osc52/v2 v2.0.1 // indirect
	github.com/cli/shurcooL-graphql v0.0.4 // indirect
	github.com/henvic/httpretty v0.0.6 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/lucasb-eyer/go-colorful v1.2.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/muesli/termenv v0.16.0 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/thlib/go-timezone-local v0.0.0-20210907160436-ef149e42d28e // indirect
	golang.org/x/sys v0.31.0 // indirect
	golang.org/x/term v0.30.0 // indirect
	golang.org/x/text v0.23.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
