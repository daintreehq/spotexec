// Package docs embeds the user documentation, so `spotexec help <page>`
// prints the pages of the installed version, offline. The Markdown files
// here are the only copy.
package docs

import "embed"

// Files holds every page in this directory, by file name (running.md).
//
//go:embed *.md
var Files embed.FS
