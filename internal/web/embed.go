// Package web embeds the built SPA (Vite output in dist/) into the binary and
// serves it with a client-routing fallback.
//
// dist/ is generated, not committed: `make ui`, `make release` and the Docker
// build all write the real Vite bundle there. Only dist/.gitkeep is tracked, so
// that this package still compiles in a fresh checkout — go:embed fails on a
// directory that does not exist.
//
// A binary built without that step embeds placeholder.html instead and says so
// on every page. The alternative, committing a generated index.html, is what
// this package used to do, and it failed badly: the committed file referenced
// hashed asset filenames that were themselves gitignored, so `go build` in a
// clean clone produced a binary serving a document whose scripts were missing.
// The SPA fallback then answered those asset requests with index.html, the
// browser refused it on MIME type, and the operator got a blank page with
// nothing in any log to explain it.
package web

import "embed"

// Dist holds the embedded SPA build. The `all:` prefix includes files whose
// names begin with `_` or `.` — Vite emits none today, but .gitkeep is one and
// hashed assets are safe either way.
//
//go:embed all:dist
var Dist embed.FS

// Placeholder is the page served when Dist carries no real build.
//
//go:embed placeholder.html
var Placeholder []byte
