package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/spotexec/spotexec/internal/config"
	"github.com/spotexec/spotexec/internal/daemon"
)

func cmdDaemon() *cobra.Command {
	c := &cobra.Command{Use: "daemon", Short: "Optional background pool manager: warm pools, demand assist, health checks, reaping"}
	run := &cobra.Command{
		Use:   "run",
		Short: "Run the pool manager in the foreground",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validate before taking the lock, so `daemon start` never reports
			// a daemon that is about to exit on a config error.
			g, err := config.LoadGlobal()
			if err != nil {
				return err
			}
			d, err := daemon.New(&config.Config{Global: g}, log.New(os.Stdout, "", log.LstdFlags).Printf)
			if err != nil {
				return err
			}
			lk, err := daemon.Lock()
			if err != nil {
				return err
			}
			defer lk.Close()
			ctx, _, stop := signals()
			defer stop()
			return d.Run(ctx)
		},
	}
	start := &cobra.Command{
		Use:   "start",
		Short: "Start the pool manager in the background",
		RunE: func(cmd *cobra.Command, args []string) error {
			if pid := daemon.Running(); pid != 0 {
				fmt.Printf("spotexec daemon already running (pid %d)\n", pid)
				return nil
			}
			self, err := os.Executable()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(daemon.Dir(), 0o700); err != nil {
				return err
			}
			logPath := filepath.Join(daemon.Dir(), "daemon.log")
			lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			p := exec.Command(self, "daemon", "run")
			p.Stdout, p.Stderr = lf, lf
			p.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := p.Start(); err != nil {
				return err
			}
			lf.Close()
			_ = p.Process.Release()
			for i := 0; i < 50; i++ {
				if pid := daemon.Running(); pid != 0 {
					fmt.Printf("spotexec daemon started (pid %d); log: %s\n", pid, logPath)
					return nil
				}
				time.Sleep(100 * time.Millisecond)
			}
			b, _ := os.ReadFile(logPath)
			return fmt.Errorf("daemon did not start; log tail:\n%s", tail(string(b), 10))
		},
	}
	stopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the background pool manager (instances keep their own idle timers)",
		RunE: func(cmd *cobra.Command, args []string) error {
			pid := daemon.Running()
			if pid == 0 {
				fmt.Println("spotexec daemon is not running")
				return nil
			}
			if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
				return err
			}
			// The daemon may wait up to 60s for launches already sent to the
			// cloud to be recorded.
			for i := 0; i < 750 && daemon.Running() != 0; i++ {
				time.Sleep(100 * time.Millisecond)
			}
			if daemon.Running() != 0 {
				return errors.New("daemon did not stop")
			}
			fmt.Printf("spotexec daemon stopped (pid %d)\n", pid)
			return nil
		},
	}
	var asJSON bool
	status := &cobra.Command{
		Use:   "status",
		Short: "Show what the pool manager sees and has done",
		RunE: func(cmd *cobra.Command, args []string) error {
			pid := daemon.Running()
			st, err := daemon.ReadStatus()
			if asJSON {
				return json.NewEncoder(os.Stdout).Encode(map[string]any{"running": pid != 0, "pid": pid, "status": st})
			}
			if pid == 0 {
				fmt.Println("spotexec daemon: not running (start it with `spotexec daemon start`)")
			} else {
				fmt.Printf("spotexec daemon: running (pid %d)", pid)
				if err == nil {
					fmt.Printf(", up %s, last pass %s ago", time.Since(st.Started).Round(time.Second), time.Since(st.Updated).Round(time.Second))
				}
				fmt.Println()
			}
			if err != nil || st == nil {
				return nil
			}
			for _, f := range st.Fleets {
				fmt.Printf("\n%s: %d instance(s) (min %d, max %d), %d queued, %d launching\n", f.Runner, len(f.Instances), f.Min, f.Max, f.Demand, f.Launching)
				for _, in := range f.Instances {
					tags := ""
					if in.Warm {
						tags += " warm"
					}
					health := "unchecked"
					switch {
					case in.Draining:
						health = "draining"
					case in.Booting:
						health = "booting"
					case in.Healthy:
						health = "healthy"
					case !in.LastOK.IsZero():
						health = "UNRESPONSIVE"
					}
					fmt.Printf("  %-22s %-12s %-8s %s%s\n", in.ID, in.Type, in.State, health, tags)
				}
				if f.LastError != "" {
					fmt.Printf("  error: %s\n", f.LastError)
				}
			}
			if n := len(st.Actions); n > 0 {
				fmt.Println("\nrecent actions:")
				from := max(0, n-10)
				for _, a := range st.Actions[from:] {
					fmt.Println("  " + a)
				}
			}
			return nil
		},
	}
	status.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	c.AddCommand(run, start, stopCmd, status)
	return c
}

func tail(s string, n int) string {
	lines := []rune(s)
	_ = lines
	out := []string{}
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	res := ""
	for _, l := range out {
		res += l + "\n"
	}
	return res
}
