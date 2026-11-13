// Command spotexec (alias sx) offloads commands from a local worktree to
// remote runners with bidirectional sync.
package main

import (
	"os"

	"github.com/spotexec/spotexec/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
