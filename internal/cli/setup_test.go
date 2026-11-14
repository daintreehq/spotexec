package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
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

func TestSetRunnerProfileTemplate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	t.Setenv("SPOTEXEC_CONFIG", p)
	if _, err := writeGlobalTemplate(p); err != nil {
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
