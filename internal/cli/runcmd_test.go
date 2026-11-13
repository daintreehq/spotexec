package cli

import (
	"os/exec"
	"testing"
)

func TestTaskCommandArgsAreData(t *testing.T) {
	cases := []struct{ tmpl, want string }{
		{`printf "<%s>" "{args}"`, "<$(echo INJECTED)><two words>"},
		{`printf "<%s>" {args}`, "<$(echo INJECTED)><two words>"},
		{`printf "<%s>" '{args}'`, "<$(echo INJECTED)><two words>"},
		{`printf "<%s>"`, "<$(echo INJECTED)><two words>"},
	}
	for _, c := range cases {
		line := taskCommand(c.tmpl, []string{"$(echo INJECTED)", "two words"})
		out, err := exec.Command("sh", "-c", line).Output()
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if string(out) != c.want {
			t.Errorf("template %q: got %q want %q", c.tmpl, out, c.want)
		}
	}
	if got := taskCommand("pnpm test {args}", nil); got != "pnpm test" {
		t.Errorf("no args: got %q", got)
	}
}
