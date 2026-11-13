package helper

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spotexec/spotexec/internal/protocol"
)

// SimDir holds injected notices in the Docker fleet simulator; it is checked
// before the real instance metadata service.
const SimDir = LifecycleDir + "/sim"

type notice struct {
	kind string // "interruption" | "rebalance"
	msg  string
}

// watchNotices runs for the life of a session holding a slot on a Spot
// runner. A rebalance recommendation marks the VM draining (running work
// finishes, nothing new is admitted); an interruption notice does the same
// and tells the client, which warms replacement capacity.
func (s *Session) watchNotices() {
	ch := make(chan notice, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pollNotices(ctx, ch)
	for {
		select {
		case n := <-ch:
			markDraining(s.layout.Root, n.msg)
			name := "spot_rebalance"
			if n.kind == "interruption" {
				name = "spot_interruption"
				s.interrupted.Store(true)
			}
			_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: name, Message: n.msg})
			if n.kind == "interruption" {
				return
			}
		case <-time.After(time.Second):
			if s.exiting.Load() {
				return
			}
		}
	}
}

func markDraining(root, msg string) {
	p := filepath.Join(root, "draining")
	if _, err := os.Stat(LifecycleDir); err == nil {
		p = DrainingFile
	}
	_ = os.WriteFile(p, []byte(msg+"\n"), 0o644)
}

// pollNotices checks the simulator files (in the simulator) or IMDSv2
// every few seconds, reporting a rebalance at most once and returning after
// an interruption. Metadata errors back off but never disable polling: a
// transient failure must not hide a later interruption notice.
func pollNotices(ctx context.Context, out chan<- notice) {
	_, err := os.Stat(SimDir)
	sim := err == nil
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	var token string
	var tokenAt time.Time
	rebalanced := false
	backoff := 3 * time.Second
	for {
		wait := 3 * time.Second
		if sim {
			if b, err := os.ReadFile(filepath.Join(SimDir, "instance-action")); err == nil {
				out <- notice{"interruption", "spot interruption notice: " + strings.TrimSpace(string(b))}
				return
			}
			if !rebalanced {
				if b, err := os.ReadFile(filepath.Join(SimDir, "rebalance")); err == nil {
					rebalanced = true
					out <- notice{"rebalance", "spot rebalance recommendation: " + strings.TrimSpace(string(b))}
				}
			}
		} else {
			if token == "" || time.Since(tokenAt) > 5*time.Hour {
				token = ""
				req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "http://169.254.169.254/latest/api/token", nil)
				req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "21600")
				if resp, err := client.Do(req); err == nil {
					b, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					if resp.StatusCode == 200 {
						token, tokenAt = string(b), time.Now()
					}
				}
			}
			if token == "" {
				wait, backoff = backoff, min(backoff*2, 60*time.Second)
			} else {
				backoff = 3 * time.Second
				if body, ok := imdsGet(ctx, client, token, "spot/instance-action"); ok {
					out <- notice{"interruption", "EC2 spot interruption notice: " + body}
					return
				}
				if !rebalanced {
					if body, ok := imdsGet(ctx, client, token, "events/recommendations/rebalance"); ok {
						rebalanced = true
						out <- notice{"rebalance", "EC2 rebalance recommendation: " + body}
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func imdsGet(ctx context.Context, c *http.Client, token, path string) (string, bool) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://169.254.169.254/latest/meta-data/"+path, nil)
	req.Header.Set("X-aws-ec2-metadata-token", token)
	resp, err := c.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b)), resp.StatusCode == 200
}
