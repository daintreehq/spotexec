package provider

import (
	"context"
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
			t.Setenv("SPOTEXEC_CONFIG_DIR", t.TempDir())
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
