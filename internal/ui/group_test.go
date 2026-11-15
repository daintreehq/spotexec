package ui

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// tape records writes to two streams in the order they arrive.
type tape struct{ log []string }

type tapeStream struct {
	t    *tape
	name string
}

func (s tapeStream) Write(b []byte) (int, error) {
	s.t.log = append(s.t.log, s.name+":"+string(b))
	return len(b), nil
}

func (t *tape) text() string { return strings.Join(t.log, "") }

func TestGroupHoldsUntilFlush(t *testing.T) {
	var tp tape
	var mu sync.Mutex
	out, errw := tapeStream{&tp, "out"}, tapeStream{&tp, "err"}
	a := NewGroup(out, errw, "[a] ", &mu)
	b := NewGroup(out, errw, "[b] ", &mu)
	a.Stdout().Write([]byte("a1\na"))
	b.Stdout().Write([]byte("b1\n"))
	a.Stdout().Write([]byte("2\n"))
	a.Stderr().Write([]byte("a-err\n"))
	b.Stderr().Write([]byte("b-err\n"))
	a.Stdout().Write([]byte("a3\n"))
	if len(tp.log) != 0 {
		t.Fatalf("output printed before flush: %q", tp.log)
	}
	if err := b.Flush(nil); err != nil {
		t.Fatal(err)
	}
	if err := a.Flush(func() { tp.log = append(tp.log, "err:a done\n") }); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"out:[b] b1\n", "err:[b] b-err\n",
		"out:[a] a1\n[a] a2\n", "err:[a] a-err\n", "out:[a] a3\n", "err:a done\n",
	}
	if strings.Join(tp.log, "|") != strings.Join(want, "|") {
		t.Fatalf("got  %q\nwant %q", tp.log, want)
	}
}

func TestGroupEndsUnfinishedLineAndPassesThroughAfterFlush(t *testing.T) {
	var tp tape
	var mu sync.Mutex
	g := NewGroup(tapeStream{&tp, "out"}, tapeStream{&tp, "err"}, "[p] ", &mu)
	g.Stdout().Write([]byte("no newline"))
	g.Flush(nil)
	g.Flush(nil) // a second flush prints nothing more
	g.Stdout().Write([]byte("late\n"))
	if got, want := tp.text(), "out:[p] no newlineout:\nout:[p] late\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestGroupSpillsToFile(t *testing.T) {
	defer func(n int) { groupMemCap = n }(groupMemCap)
	groupMemCap = 64
	var tp tape
	var mu sync.Mutex
	g := NewGroup(tapeStream{&tp, "out"}, tapeStream{&tp, "err"}, "[s] ", &mu)
	var want strings.Builder
	for i := range 200 {
		line := fmt.Sprintf("line %d\n", i)
		if i%3 == 0 {
			g.Stderr().Write([]byte(line))
		} else {
			g.Stdout().Write([]byte(line))
		}
	}
	if g.spill == nil {
		t.Fatal("output past the memory cap was not moved to a file")
	}
	if err := g.Flush(nil); err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		stream := "out:"
		if i%3 == 0 {
			stream = "err:"
		}
		want.WriteString(fmt.Sprintf("%s[s] line %d\n", stream, i))
	}
	// Consecutive same-stream lines arrive in one write; compare per line.
	var got strings.Builder
	for _, e := range tp.log {
		stream, body, _ := strings.Cut(e, ":")
		for _, l := range strings.SplitAfter(body, "\n") {
			if l != "" {
				got.WriteString(stream + ":" + l)
			}
		}
	}
	if got.String() != want.String() {
		t.Fatalf("spilled output replayed wrong:\n%s", got.String()[:min(400, got.Len())])
	}
}

func TestGroupPrintsWhatItCantHold(t *testing.T) {
	defer func(n int) { groupMemCap = n }(groupMemCap)
	groupMemCap = 16
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	var tp tape
	var mu sync.Mutex
	g := NewGroup(tapeStream{&tp, "out"}, tapeStream{&tp, "err"}, "[x] ", &mu)
	g.Stdout().Write([]byte("one\n"))
	if len(tp.log) != 0 {
		t.Fatalf("printed before the cap: %q", tp.log)
	}
	// Past the cap the temporary file can't be made: what was held goes out,
	// then the rest as it comes.
	g.Stderr().Write([]byte("two two two two\n"))
	g.Stdout().Write([]byte("three\n"))
	if got, want := tp.text(), "out:[x] one\nerr:[x] two two two two\nout:[x] three\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if err := g.Flush(nil); err == nil {
		t.Error("Flush didn't report the failure to hold output")
	}
}

func TestGroupConcurrentParts(t *testing.T) {
	defer func(n int) { groupMemCap = n }(groupMemCap)
	groupMemCap = 256 // some parts spill, some don't
	var mu sync.Mutex
	var out strings.Builder
	w := writerFunc(func(b []byte) (int, error) { return out.Write(b) })
	var wg sync.WaitGroup
	for p := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g := NewGroup(w, w, fmt.Sprintf("[%d] ", p), &mu)
			for i := range 20 * (p + 1) {
				fmt.Fprintf(g.Stdout(), "%d\n", i)
			}
			g.Flush(nil)
		}()
	}
	wg.Wait()
	// Each part's lines are one contiguous run, in order.
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	seen := map[string]bool{}
	for i := 0; i < len(lines); {
		part, _, _ := strings.Cut(lines[i], " ")
		if seen[part] {
			t.Fatalf("part %s printed in two blocks", part)
		}
		seen[part] = true
		n := 0
		for ; i < len(lines) && strings.HasPrefix(lines[i], part+" "); i++ {
			if want := fmt.Sprintf("%s %d", part, n); lines[i] != want {
				t.Fatalf("line %q, want %q", lines[i], want)
			}
			n++
		}
	}
	if len(seen) != 8 {
		t.Fatalf("got %d blocks", len(seen))
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
