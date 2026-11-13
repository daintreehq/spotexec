// Command spotexec-helper is the remote half of spotexec. The CLI uploads it
// to each runner and drives it over SSH; it is not meant to be run by hand.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spotexec/spotexec/internal/helper"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: spotexec-helper rpc|status|unlock|prune|version")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	workRoot := fs.String("work-root", "~/.cache/spotexec", "work root")
	token := fs.Int("token", 0, "token to unlock")
	all := fs.Bool("all", false, "unlock all")
	olderThan := fs.Duration("older-than", 7*24*time.Hour, "prune age")
	dryRun := fs.Bool("dry-run", false, "prune dry run")
	src := fs.String("src", "", "cache-put: directory to archive")
	lockPath := fs.String("lock", "", "cache-put: dependency key lock to hold shared")
	_ = fs.Parse(os.Args[2:])
	root, err := helper.ExpandRoot(*workRoot)
	if err != nil {
		fail(err)
	}
	switch os.Args[1] {
	case "rpc":
		// SIGHUP/SIGTERM (sshd teardown, unlock) close stdin semantics: exit
		// via the normal path so locks and child groups are cleaned up.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT, syscall.SIGPIPE)
		go func() {
			<-sig
			os.Stdin.Close()
		}()
		if err := helper.Serve(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "spotexec-helper:", err)
			os.Exit(1)
		}
	case "status":
		fail(helper.PrintStatus(os.Stdout, root))
	case "unlock":
		fail(helper.Unlock(os.Stdout, root, *token, *all))
	case "prune":
		fail(helper.Prune(os.Stdout, root, *olderThan, *dryRun))
	case "drain":
		fail(helper.Drain(os.Stdout, root, "retired by the spotexec pool manager"))
	case "keepalive":
		// Used by `spotexec daemon` to hold warm-pool instances past their
		// idle timeout: refreshes the activity the idle watchdog checks.
		if err := helper.Keepalive(os.Stdout, root); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "image-push":
		fail(helper.ImagePush(root))
	case "cache-put":
		fail(helper.CachePut(root, *src, *lockPath))
	case "version":
		fmt.Println(helper.Version)
	default:
		fail(fmt.Errorf("unknown command %q", os.Args[1]))
	}
}

func fail(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "spotexec-helper:", err)
		os.Exit(1)
	}
}
