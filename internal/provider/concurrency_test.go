package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
)

// Shared providers serve concurrent goroutines (AWSGlobal's regions, the
// daemon, acquire); the lazily filled owner must be safe under -race and
// agree across callers.
func TestOwnerTagConcurrent(t *testing.T) {
	for _, tc := range []struct {
		name string
		tag  func() (string, error)
	}{
		{"aws", (&AWSSpot{r: &config.Runner{Name: "r"}}).ownerTag},
		{"sim", (&Sim{r: &config.Runner{Name: "r"}}).ownerTag},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("SPOTEXEC_CONFIG_DIR", dir)
			const n = 16
			got := make([]string, n)
			var wg sync.WaitGroup
			for i := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					o, err := tc.tag()
					if err != nil {
						t.Error(err)
					}
					got[i] = o
				}()
			}
			wg.Wait()
			for i, o := range got {
				if o == "" || o != got[0] {
					t.Fatalf("caller %d got owner %q, caller 0 got %q", i, o, got[0])
				}
			}
			b, err := os.ReadFile(filepath.Join(dir, "owner-id"))
			if err != nil || strings.TrimSpace(string(b)) != got[0] {
				t.Fatalf("owner-id on disk = %q, %v; callers got %q", b, err, got[0])
			}
		})
	}
}

// SSH's chosen host is read by Status/Up and reset by Forget from different
// goroutines.
func TestSSHChosenConcurrent(t *testing.T) {
	s := &SSH{r: &config.Runner{Name: "r", User: "u", Hosts: []string{"h1"}}}
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			if info, err := s.Status(ctx); err != nil || info.Target.Host != "h1" {
				t.Errorf("Status = %+v, %v", info, err)
			}
		}()
		go func() {
			defer wg.Done()
			if info, err := s.Up(ctx, nil); err != nil || info.Target.Host != "h1" {
				t.Errorf("Up = %+v, %v", info, err)
			}
		}()
		go func() { defer wg.Done(); s.Forget() }()
	}
	wg.Wait()
}

// blockedPick starts a pick whose selector, once entered, waits for release
// before choosing hosts[idx]. Cleanup releases it if the test ends early.
func blockedPick(t *testing.T, s *SSH, idx int) (release func(), done <-chan Target) {
	t.Helper()
	entered, rel := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(rel) }) }
	t.Cleanup(release)
	out := make(chan Target, 1)
	go func() {
		tg, err := s.pickWith(context.Background(), func(context.Context) (int, error) {
			close(entered)
			<-rel
			return idx, nil
		})
		if err != nil {
			t.Error(err)
		}
		out <- tg
	}()
	<-entered
	return release, out
}

func cached(s *SSH) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chosen == nil {
		return ""
	}
	return s.chosen.Host
}

func choose(idx int) func(context.Context) (int, error) {
	return func(context.Context) (int, error) { return idx, nil }
}

// A cached host is reused without selecting; Forget drops it, so the next
// pick selects afresh; a pick that was dialing when Forget ran returns its
// host but doesn't cache it, since Forget means the old answer may be stale.
// The rounds overlap one and then two Forgets, so every Forget invalidates.
func TestSSHForget(t *testing.T) {
	ctx := context.Background()
	s := &SSH{r: &config.Runner{Name: "r", Hosts: []string{"a", "b"}}}
	if tg, err := s.pickWith(ctx, choose(0)); err != nil || tg.Host != "a" || cached(s) != "a" {
		t.Fatalf("first pick = %+v, %v; cached %q", tg, err, cached(s))
	}
	if tg, err := s.pickWith(ctx, func(context.Context) (int, error) {
		t.Fatal("selector ran with a host cached")
		return 0, nil
	}); err != nil || tg.Host != "a" {
		t.Fatalf("cached pick = %+v, %v", tg, err)
	}
	s.Forget()
	if tg, err := s.pickWith(ctx, choose(1)); err != nil || tg.Host != "b" || cached(s) != "b" {
		t.Fatalf("pick after Forget = %+v, %v; cached %q, want b", tg, err, cached(s))
	}
	for round := range 2 {
		s.Forget()
		release, done := blockedPick(t, s, 0)
		for range round + 1 {
			s.Forget()
		}
		release()
		if tg := <-done; tg.Host != "a" {
			t.Fatalf("round %d: overtaken pick returned %q, want a", round, tg.Host)
		}
		if h := cached(s); h != "" {
			t.Fatalf("round %d: pick begun before Forget was cached: %q", round, h)
		}
	}
}

// Two overlapping picks settle on one host: the later finisher returns the
// winner's choice rather than replacing it.
func TestSSHPickKeepsFirstSettled(t *testing.T) {
	ctx := context.Background()
	s := &SSH{r: &config.Runner{Name: "r", Hosts: []string{"a", "b"}}}
	release, done := blockedPick(t, s, 1)
	if first, err := s.pickWith(ctx, choose(0)); err != nil || first.Host != "a" {
		t.Fatalf("first pick = %+v, %v", first, err)
	}
	release()
	if tg := <-done; tg.Host != "a" {
		t.Fatalf("overlapping pick returned %q, want the settled a", tg.Host)
	}
	if h := cached(s); h != "a" {
		t.Fatalf("cached host = %q, want a", h)
	}
}
