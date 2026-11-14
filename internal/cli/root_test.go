package cli

import "testing"

// A command missing from subcommands is run remotely as `sx <name>`
// instead (e.g. `spotexec prices` would provision a VM to run "prices").
func TestEveryCommandIsASubcommand(t *testing.T) {
	root := newRoot()
	if c, _, err := root.Find([]string{"prices"}); err != nil || c.Name() != "prices" {
		t.Fatalf("prices is not registered: %v", err)
	}
	for _, c := range root.Commands() {
		if !subcommands[c.Name()] {
			t.Errorf("%q is missing from subcommands", c.Name())
		}
	}
}
