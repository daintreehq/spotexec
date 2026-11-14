// Command spotexec (alias sx) offloads commands from a local worktree to
// remote runners with bidirectional sync.
package main

import (
	"os"
	"path/filepath"

	"github.com/daintreehq/spotexec/internal/cli"
)

func main() {
	os.Exit(cli.MainAs(filepath.Base(os.Args[0]), os.Args[1:]))
}
