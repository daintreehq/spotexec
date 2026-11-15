package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/ui"
)

// pull reads SPOTEXEC_JSON as a run does: a value that isn't a boolean is
// rejected before anything is loaded, and --json either way beats it.
func TestPullRejectsBadJSONEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	pull := func(args ...string) error {
		c := cmdPull()
		c.SetArgs(args)
		c.SetOut(&bytes.Buffer{})
		c.SetErr(&bytes.Buffer{})
		c.SilenceUsage, c.SilenceErrors = true, true
		return c.Execute()
	}
	t.Setenv("SPOTEXEC_JSON", "bogus")
	if err := pull("x"); err == nil || !strings.Contains(err.Error(), "SPOTEXEC_JSON must be") {
		t.Fatalf("SPOTEXEC_JSON=bogus: %v", err)
	}
	for _, flag := range []string{"--json", "--json=false"} {
		if err := pull(flag, "x"); err != nil && strings.Contains(err.Error(), "SPOTEXEC_JSON") {
			t.Fatalf("%s still read SPOTEXEC_JSON: %v", flag, err)
		}
	}
	t.Setenv("SPOTEXEC_JSON", "yes")
	if err := pull("x"); err != nil && strings.Contains(err.Error(), "SPOTEXEC_JSON") {
		t.Fatalf("SPOTEXEC_JSON=yes rejected: %v", err)
	}
}

// pullMode: --json either way beats SPOTEXEC_JSON, the usual boolean
// spellings turn it on or off, and --json=false beats SPOTEXEC_AGENT=1.
func TestPullMode(t *testing.T) {
	t.Setenv("SPOTEXEC_AGENT", "")
	for _, c := range []struct {
		env     string
		asJSON  bool
		jsonSet bool
		want    ui.Mode
	}{
		{"1", false, false, ui.ModeJSON},
		{"true", false, false, ui.ModeJSON},
		{"YES", false, false, ui.ModeJSON},
		{"", false, false, ui.ModeAuto},
		{"no", false, false, ui.ModeAuto},
		{"0", false, false, ui.ModeAuto},
		{"yes", false, true, ui.ModeAuto},  // --json=false
		{"bogus", true, true, ui.ModeJSON}, // --json: the variable isn't read
	} {
		t.Setenv("SPOTEXEC_JSON", c.env)
		got, err := pullMode(c.asJSON, c.jsonSet)
		if err != nil || got != c.want {
			t.Errorf("SPOTEXEC_JSON=%q json=%v set=%v: %v, %v (want %v)", c.env, c.asJSON, c.jsonSet, got, err, c.want)
		}
	}
	t.Setenv("SPOTEXEC_JSON", "")
	t.Setenv("SPOTEXEC_AGENT", "1")
	if got, _ := pullMode(false, true); got != ui.ModePlain {
		t.Errorf("--json=false with SPOTEXEC_AGENT=1: %v", got)
	}
}
