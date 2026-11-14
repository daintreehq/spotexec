package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
)

func TestSetRunnerProfileText(t *testing.T) {
	in := `version = 1
default_pool = ["aws", "big"]

[runners.aws]
type = "aws-spot"
region = "us-east-1"
# profile = "default"          # AWS CLI profile
bootstrap_script = """
echo hi
"""

[runners."big"]   # comment
type = "aws-spot"
profile = "old"

[runners.box]
type = "ssh"
host = "me@box"
`
	out, err := setRunnerProfileText(in, []string{"aws", "big"}, "spotexec")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"region = \"us-east-1\"\nprofile = \"spotexec\"   # set by `spotexec setup`\nbootstrap_script",
		"[runners.\"big\"]   # comment\ntype = \"aws-spot\"\nprofile = \"spotexec\"\n",
		"[runners.box]\ntype = \"ssh\"\nhost = \"me@box\"\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// No profile line at all: inserted after type.
	out, err = setRunnerProfileText("[runners.a]\ntype = \"aws-spot\"\nregion = \"x\"\n", []string{"a"}, "p")
	if err != nil || out != "[runners.a]\ntype = \"aws-spot\"\nprofile = \"p\"   # set by `spotexec setup`\nregion = \"x\"\n" {
		t.Fatalf("got %q %v", out, err)
	}
	if _, err := setRunnerProfileText("[runners.a]\n", []string{"zz"}, "p"); err == nil {
		t.Fatal("missing runner not reported")
	}
}

// Lines inside multi-line strings and arrays are values, not tables or keys,
// however they look; everything but the profile line stays as it was.
func TestSetRunnerProfileTextMultiline(t *testing.T) {
	in := `version = 1 # "quoted [x]" comment

[runners.aws]   # main
type = "aws-spot"
bootstrap_script = """
[ -f /etc/foo ] && echo "\"" '
profile = "inside"
[runners.fake]
type = "x"
"""
user_bootstrap_script = '''
[ -d ~/.cache ] || mkdir ~/.cache
# profile = "nope"
'''
tags = [
  ["a", "b"],
  { k = "[v" },
]
region = "us-east-1"

[runners.other]
type = "aws-spot"
profile = """
old"""   # odd but valid
max_slots = 2
`
	out, err := setRunnerProfileText(in, []string{"aws", "other"}, "spotexec")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(in, "type = \"aws-spot\"\nbootstrap_script", "type = \"aws-spot\"\nprofile = \"spotexec\"   # set by `spotexec setup`\nbootstrap_script", 1)
	want = strings.Replace(want, "profile = \"\"\"\nold\"\"\"   # odd but valid\n", "profile = \"spotexec\"\n", 1)
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	// A runner whose header only appears inside a string isn't there.
	if _, err := setRunnerProfileText(in, []string{"fake"}, "p"); err == nil {
		t.Fatal("found a runner inside a string")
	}
	// CRLF files stay CRLF.
	out, err = setRunnerProfileText("[runners.a]\r\ntype = \"aws-spot\"\r\n", []string{"a"}, "p")
	if err != nil || out != "[runners.a]\r\ntype = \"aws-spot\"\r\nprofile = \"p\"   # set by `spotexec setup`\r\n" {
		t.Fatalf("got %q %v", out, err)
	}
	// ... including ones without a final newline.
	for in, want := range map[string]string{
		"[runners.a]\r\ntype = \"aws-spot\"":        "[runners.a]\r\ntype = \"aws-spot\"\r\nprofile = \"p\"   # set by `spotexec setup`",
		"[runners.a]\r\nprofile = \"old\"":          "[runners.a]\r\nprofile = \"p\"",
		"[runners.a]\r\nprofile = \"old\"\r\nx = 1": "[runners.a]\r\nprofile = \"p\"\r\nx = 1",
		"[runners.a]\r\n# profile = \"x\"\r\n":      "[runners.a]\r\nprofile = \"p\"   # set by `spotexec setup`\r\n",
		"x = 1\r\n[runners.a]":                      "x = 1\r\n[runners.a]\r\nprofile = \"p\"   # set by `spotexec setup`",
		"[runners.a]\ntype = \"aws-spot\"":          "[runners.a]\ntype = \"aws-spot\"\nprofile = \"p\"   # set by `spotexec setup`",
	} {
		if out, err := setRunnerProfileText(in, []string{"a"}, "p"); err != nil || out != want {
			t.Errorf("setRunnerProfileText(%q) = %q, %v; want %q", in, out, err, want)
		}
	}
}

func TestSetRunnerProfileMultilineReloads(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	t.Setenv("SPOTEXEC_CONFIG", p)
	in := `version = 1
default_pool = ["aws"]

[runners.aws]
type = "aws-spot"
region = "us-east-1"
bootstrap_script = """
[ -f /etc/foo ] && echo hi
[ -d /opt ] || mkdir /opt
"""
instance_types = ["c7a.xlarge"]
`
	if err := os.WriteFile(p, []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadGlobal(); err != nil {
		t.Fatalf("fixture doesn't load: %v", err)
	}
	if changed, err := setRunnerProfile(p, []string{"aws"}, "spotexec"); err != nil || len(changed) != 1 {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	g, err := config.LoadGlobal()
	if err != nil || g.Runners["aws"].Profile != "spotexec" || g.Runners["aws"].BootstrapScript != "[ -f /etc/foo ] && echo hi\n[ -d /opt ] || mkdir /opt\n" {
		t.Fatalf("reloaded %+v, %v", g, err)
	}
}

func TestSetRunnerProfileTemplate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	t.Setenv("SPOTEXEC_CONFIG", p)
	if _, _, err := writeGlobalTemplate(p, ""); err != nil {
		t.Fatal(err)
	}
	g, err := config.LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, r := range awsRunners(g) {
		names = append(names, r.Name)
	}
	changed, err := setRunnerProfile(p, names, "spotexec")
	if err != nil || len(changed) != 1 {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	g, _ = config.LoadGlobal()
	if g.Runners["aws"].Profile != "spotexec" {
		t.Fatalf("profile = %q", g.Runners["aws"].Profile)
	}
	// Idempotent.
	if changed, err := setRunnerProfile(p, names, "spotexec"); err != nil || len(changed) != 0 {
		t.Fatalf("second run changed %v, %v", changed, err)
	}
	b, _ := os.ReadFile(p)
	if strings.Count(string(b), "profile = \"spotexec\"") != 1 {
		t.Fatalf("config:\n%s", b)
	}
}

func TestDefaultIAMUser(t *testing.T) {
	u := defaultIAMUser()
	if !strings.HasPrefix(u, "spotexec-") || len(u) > 64 || strings.ContainsAny(u, ". _") {
		t.Fatalf("defaultIAMUser() = %q", u)
	}
}

func TestParseHostChoice(t *testing.T) {
	found := []string{"studio-01.local", "studio-02.local", "mini-01.local"}
	for in, want := range map[string]string{
		"":                       "",
		"1 3":                    "studio-01.local,mini-01.local",
		"all":                    "studio-01.local,studio-02.local,mini-01.local",
		"2, box.example.com 2 9": "studio-02.local,box.example.com",
		"me@studio-04.local\n":   "me@studio-04.local",
	} {
		if got := strings.Join(parseHostChoice(in, found), ","); got != want {
			t.Errorf("parseHostChoice(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckSSHHost(t *testing.T) {
	for _, h := range []string{"studio-01.local", "me@build-box", "first.last@host.local", "10.0.0.5", "builder", "[::1]"} {
		if err := checkSSHHost(h); err != nil {
			t.Errorf("checkSSHHost(%q) = %v", h, err)
		}
	}
	for _, h := range []string{"", "-oProxyCommand=touch /tmp/x", "-v", "a b", "a\tb", "host\n", "a\x00b", "a\x1bb", "a b"} {
		if err := checkSSHHost(h); err == nil {
			t.Errorf("checkSSHHost(%q) accepted", h)
		}
	}
	if err := checkSSHHosts([]string{"ok", "-oProxyCommand=x"}); err == nil || !strings.Contains(err.Error(), "-oProxyCommand") {
		t.Errorf("checkSSHHosts = %v", err)
	}
}

func TestSSHArgsEndOptions(t *testing.T) {
	r := remoteSetup{host: "me@box", ctlDir: "/tmp/x"}
	args := r.sshArgs("true")
	if len(args) < 3 || args[len(args)-3] != "--" || args[len(args)-2] != "me@box" {
		t.Fatalf("sshArgs = %q", args)
	}
}

func TestHostListDropsUnsafeNames(t *testing.T) {
	var l hostList
	for _, h := range []string{"studio-01.local.", "-oProxyCommand=sh -c x", "Studio-01.local", "", "  ", "mini 01", " builder.local", "builder.local\n", "\tbox", "box\u2003", "mini-01.local", "localhost"} {
		l.add(h)
	}
	if got := strings.Join(l.hosts, ","); got != "studio-01.local,mini-01.local" {
		t.Fatalf("hosts = %q", got)
	}
}

func TestMatchMachine(t *testing.T) {
	ms := []provider.Machine{
		{User: "spotexec-studio-01", Host: "studio-01", MachineID: "A"},
		{User: "spotexec-studio-01-x1", Host: "Studio-01", MachineID: "B"},
		{User: "spotexec-mini-01", Host: "mini-01", MachineID: "C"},
	}
	saved := []savedMachine{
		{Host: "builder", MachineID: "B"},
		{Host: "old-alias", Hostname: "mini-01.local"}, // recorded before machine IDs
		{Host: "plain"},
	}
	for target, want := range map[string]string{
		"spotexec-studio-01":       "spotexec-studio-01", // an exact IAM user name always wins
		"SPOTEXEC-STUDIO-01-X1":    "spotexec-studio-01-x1",
		"studio-01":                "error",
		"studio-01.local":          "error",
		"first.last@mini-01.local": "spotexec-mini-01",
		"mini-01":                  "spotexec-mini-01",
		"builder":                  "spotexec-studio-01-x1", // the machine the alias reached
		"old-alias":                "spotexec-mini-01",
		"plain":                    "error",
		"nope":                     "error",
	} {
		hit, err := matchMachine(ms, saved, target)
		got := "error"
		if err == nil {
			got = hit.User
		}
		if got != want {
			t.Errorf("matchMachine(%q) = %s (%v), want %s", target, got, err, want)
		}
	}
	if _, err := matchMachine(ms, saved, "studio-01"); err == nil || !strings.Contains(err.Error(), "spotexec-studio-01 and spotexec-studio-01-x1") {
		t.Errorf("err = %v", err)
	}
	// A destination recorded as a machine that's gone never falls back to
	// another machine of that name.
	saved = append(saved, savedMachine{Host: "mini-01", MachineID: "Z"})
	if hit, err := matchMachine(ms, saved, "mini-01"); err == nil {
		t.Errorf("matched %s", hit.User)
	}
	// ... not even an untagged one.
	ms = append(ms, provider.Machine{User: "spotexec-legacy", Host: "studio-09"})
	saved = append(saved, savedMachine{Host: "nine", MachineID: "Z", Hostname: "studio-09.local"})
	if hit, err := matchMachine(ms, saved, "nine"); err == nil {
		t.Errorf("matched %s", hit.User)
	}
}

// Revoking a machine removes every destination that reached it, and only
// those; entries the list can't place are left and reported.
func TestRevokeForgetsEveryDestination(t *testing.T) {
	t.Setenv("SPOTEXEC_CONFIG_DIR", t.TempDir())
	ms := []provider.Machine{
		{User: "spotexec-studio-01", Host: "studio-01", MachineID: "A"},
		{User: "spotexec-studio-01-x1", Host: "studio-01", MachineID: "B"},
		{User: "spotexec-mini-01", Host: "mini-01", MachineID: "C"},
		{User: "spotexec-box", Host: "box", MachineID: "D"},
	}
	rememberMachine("builder", "A", "studio-01.local")
	rememberMachine("me@studio-01b.local", "B", "studio-01.local")
	rememberMachine("mini-01.local", "C", "mini-01.local")
	// The old schema: just the destination.
	if err := saveMachines(append(loadMachines(), savedMachine{Host: "old-alias"}, savedMachine{Host: "studio-01.local"}, savedMachine{Host: "box.local"})); err != nil {
		t.Fatal(err)
	}
	var unsure []string
	for _, e := range loadMachines() {
		if _, u := rosterMatch(e, ms[0], ms, "spotexec-studio-01"); u {
			unsure = append(unsure, e.Host)
		}
	}
	gone := forgetMachine(func(e savedMachine) bool { m, _ := rosterMatch(e, ms[0], ms, "spotexec-studio-01"); return m })
	if strings.Join(gone, ",") != "builder" || strings.Join(unsure, ",") != "old-alias,studio-01.local" {
		t.Fatalf("forgot %v, unsure about %v", gone, unsure)
	}
	// A legacy entry whose name only one machine has goes with it.
	gone = forgetMachine(func(e savedMachine) bool { m, _ := rosterMatch(e, ms[3], ms, "box"); return m })
	if strings.Join(gone, ",") != "box.local" {
		t.Fatalf("forgot %v", gone)
	}
	// So does the destination revoke was given.
	gone = forgetMachine(func(e savedMachine) bool { m, _ := rosterMatch(e, ms[1], ms, "studio-01.local"); return m })
	if strings.Join(gone, ",") != "me@studio-01b.local,studio-01.local" {
		t.Fatalf("forgot %v", gone)
	}
}

// One entry per machine: a new destination for a known machine replaces
// the old one, and an entry learns its machine ID.
func TestRememberMachineIdentity(t *testing.T) {
	t.Setenv("SPOTEXEC_CONFIG_DIR", t.TempDir())
	rememberMachine("studio-01.local", "", "")
	rememberMachine("studio-01.local", "A", "studio-01")
	rememberMachine("builder", "A", "studio-01")
	rememberMachine("mini", "", "")
	ms := loadMachines()
	if len(ms) != 2 || ms[0].Host != "builder" || ms[0].MachineID != "A" || ms[0].Hostname != "studio-01" || ms[1].Host != "mini" {
		t.Fatalf("machines %+v", ms)
	}
	// The destination now reaches another machine whose ID isn't known:
	// the old identity goes.
	rememberMachine("builder", "", "mini-01")
	if ms := loadMachines(); ms[0].MachineID != "" || ms[0].Hostname != "mini-01" {
		t.Fatalf("machines %+v", ms)
	}
}

// Revoking a user without a machine ID tag: entries of known machines stay,
// others are reported.
func TestRosterMatchUntaggedUser(t *testing.T) {
	ms := []provider.Machine{{User: "spotexec-legacy"}, {User: "spotexec-mini", Host: "mini", MachineID: "C"}}
	for _, c := range []struct {
		e             savedMachine
		match, unsure bool
	}{
		{savedMachine{Host: "alias", MachineID: "Q", Hostname: "old"}, false, true},
		{savedMachine{Host: "mini.local", MachineID: "C", Hostname: "mini"}, false, false},
		{savedMachine{Host: "spotexec-legacy", MachineID: "Q"}, true, false},
		{savedMachine{Host: "plain"}, false, true},
	} {
		if m, u := rosterMatch(c.e, ms[0], ms, "spotexec-legacy"); m != c.match || u != c.unsure {
			t.Errorf("rosterMatch(%+v) = %v, %v", c.e, m, u)
		}
	}
}
