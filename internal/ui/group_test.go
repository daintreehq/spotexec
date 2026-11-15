package ui

import (
	"fmt"
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
	if err := b.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"out:[b] b1\n", "err:[b] b-err\n",
		"out:[a] a1\n[a] a2\n", "err:[a] a-err\n", "out:[a] a3\n",
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
	g.Flush()
	g.Flush() // a second flush prints nothing more
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
	if err := g.Flush(); err != nil {
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
