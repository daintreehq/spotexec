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

// A pick that was dialing when Forget ran returns its host but doesn't cache
// it: Forget means the old answer may be stale.
func TestSSHPickOvertakenByForget(t *testing.T) {
	s := &SSH{r: &config.Runner{Name: "r", Hosts: []string{"a", "b"}}}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan Target)
	go func() {
		tg, err := s.pickWith(context.Background(), func(context.Context) (int, error) {
			close(entered)
			<-release
			return 1, nil
		})
		if err != nil {
			t.Error(err)
		}
		done <- tg
	}()
	<-entered
	s.Forget()
	close(release)
	if tg := <-done; tg.Host != "b" {
		t.Fatalf("pick returned %q, want b", tg.Host)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chosen != nil {
		t.Fatalf("pick begun before Forget was cached: %+v", *s.chosen)
	}
}

// Two overlapping picks settle on one host: the later finisher returns the
// winner's choice rather than replacing it.
func TestSSHPickKeepsFirstSettled(t *testing.T) {
	s := &SSH{r: &config.Runner{Name: "r", Hosts: []string{"a", "b"}}}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan Target)
	go func() {
		tg, _ := s.pickWith(context.Background(), func(context.Context) (int, error) {
			close(entered)
			<-release
			return 1, nil
		})
		done <- tg
	}()
	<-entered
	first, err := s.pickWith(context.Background(), func(context.Context) (int, error) { return 0, nil })
	if err != nil || first.Host != "a" {
		t.Fatalf("first pick = %+v, %v", first, err)
	}
	close(release)
	if tg := <-done; tg.Host != "a" {
		t.Fatalf("overlapping pick returned %q, want the settled a", tg.Host)
	}
	if tg, _ := s.pick(context.Background()); tg.Host != "a" {
		t.Fatalf("cached host = %q, want a", tg.Host)
	}
}
