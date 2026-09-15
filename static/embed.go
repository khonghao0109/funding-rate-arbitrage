// Package static is the operator page (PLAN Q17): the four-tab UI that
// cmd/execportal serves on loopback, embedded into that binary at build time so
// the page that can place orders loads nothing from disk or from another host.
//
// The directory has a second reader. cmd/scanner serves ./static/ FROM DISK
// (http.FileServer, cwd = the repository root), so every file here — this one
// included — is also what the scanner's port answers. The page only works
// behind cmd/execportal: it needs /api/status and the relay routes, which the
// scanner does not have, and it says so instead of polling a 404.
//
// Nothing here imports the repository or runs code; a test in cmd/execportal
// keeps it that way, because this package is linked into the binary that holds
// the testnet credentials.
package static

import (
	"embed"
	"io/fs"
)

// Every served directory is named: a new one added here without a pattern
// would be missing from the binary, and cmd/execportal's test compares this
// tree against the directory on disk.
//
//go:embed index.html css js fonts vendor research
var files embed.FS

// FS returns the page's files with index.html at the root.
func FS() fs.FS {
	return files
}
