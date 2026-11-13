package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/spotexec/spotexec/internal/config"
	"github.com/spotexec/spotexec/internal/provider"
)

// cmdSim drives the Docker-based Spot fleet simulator (type = "sim").
func cmdSim() *cobra.Command {
	c := &cobra.Command{Use: "sim", Short: "Spot fleet simulator controls (type = \"sim\" runners)", Hidden: true}
	pick := func(args []string) (*provider.Sim, string, error) {
		cfg, err := loadGlobal()
		if err != nil {
			return nil, "", err
		}
		var r *config.Runner
		if len(args) > 0 {
			r, err = cfg.Runner(args[0])
		} else {
			for _, n := range cfg.RunnerNames() {
				if cfg.Global.Runners[n].Type == config.TypeSim {
					r = cfg.Global.Runners[n]
					break
				}
			}
			if r == nil {
				err = errors.New("no sim runner configured")
			}
		}
		if err != nil {
			return nil, "", err
		}
		p, err := provider.New(r)
		if err != nil {
			return nil, "", err
		}
		sim, ok := p.(*provider.Sim)
		if !ok {
			return nil, "", fmt.Errorf("runner %s is not a sim runner", r.Name)
		}
		id := ""
		if len(args) > 1 {
			id = args[1]
		}
		return sim, id, nil
	}
	firstRunning := func(cmd *cobra.Command, sim *provider.Sim, id string) (string, error) {
		if id != "" {
			return id, nil
		}
		insts, err := sim.List(cmd.Context())
		if err != nil {
			return "", err
		}
		for _, in := range insts {
			if in.State == provider.StateRunning {
				return in.ID, nil
			}
		}
		return "", errors.New("no running simulated instance")
	}
	var after time.Duration
	interrupt := &cobra.Command{
		Use:   "interrupt [runner] [instance]",
		Short: "Send a spot interruption notice, then terminate the instance after --after",
		RunE: func(cmd *cobra.Command, args []string) error {
			sim, id, err := pick(args)
			if err != nil {
				return err
			}
			if id, err = firstRunning(cmd, sim, id); err != nil {
				return err
			}
			fmt.Printf("interrupting %s (terminates in %s)\n", id, after)
			return sim.Interrupt(cmd.Context(), id, after)
		},
	}
	interrupt.Flags().DurationVar(&after, "after", 10*time.Second, "time between the notice and termination (AWS: 2m)")
	rebalance := &cobra.Command{
		Use:   "rebalance [runner] [instance]",
		Short: "Send a rebalance recommendation (the instance drains)",
		RunE: func(cmd *cobra.Command, args []string) error {
			sim, id, err := pick(args)
			if err != nil {
				return err
			}
			if id, err = firstRunning(cmd, sim, id); err != nil {
				return err
			}
			fmt.Printf("rebalance recommendation sent to %s\n", id)
			return sim.Rebalance(cmd.Context(), id)
		},
	}
	build := &cobra.Command{
		Use:   "build [runner]",
		Short: "Build the simulator image",
		RunE: func(cmd *cobra.Command, args []string) error {
			sim, _, err := pick(args)
			if err != nil {
				return err
			}
			return sim.EnsureImage(cmd.Context())
		},
	}
	c.AddCommand(interrupt, rebalance, build)
	return c
}
