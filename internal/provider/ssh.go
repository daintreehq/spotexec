package provider

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// SSH is a static runner reachable over SSH. With several hosts (e.g. LAN
// address first, Tailscale name second) it dials them in parallel and prefers
// the earliest-listed one if it answers within a 500ms grace window.
type SSH struct {
	r *config.Runner

	mu     sync.Mutex
	chosen *Target // the host pick settled on; nil until then or after Forget
	gen    int     // bumped by Forget, so a pick begun before it isn't kept
}

func (s *SSH) Runner() *config.Runner { return s.r }

func (s *SSH) target(host string) Target {
	return Target{Host: host, User: s.r.User, IdentityFile: config.ExpandHome(s.r.IdentityFile), Options: s.r.SSHOptions}
}

func (s *SSH) Status(ctx context.Context) (Info, error) {
	t, err := s.pick(ctx)
	if err != nil {
		return Info{State: StateUnknown, Detail: err.Error()}, nil
	}
	return Info{State: StateRunning, Target: t}, nil
}

func (s *SSH) Up(ctx context.Context, log Logger) (Info, error) {
	t, err := s.pick(ctx)
	if err != nil {
		return Info{State: StateUnknown}, err
	}
	return Info{State: StateRunning, Target: t}, nil
}

func (s *SSH) Down(ctx context.Context, log Logger) error { return nil }

func (s *SSH) Forget() {
	s.mu.Lock()
	s.chosen = nil
	s.gen++
	s.mu.Unlock()
}

func (s *SSH) Doctor(ctx context.Context) []Check {
	var out []Check
	for _, h := range s.r.Hosts {
		addr, err := resolveSSH(ctx, h, s.r.SSHOptions)
		if err != nil {
			out = append(out, Check{Name: "resolve " + h, Msg: err.Error()})
			continue
		}
		c, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", addr)
		if err != nil {
			out = append(out, Check{Name: "tcp " + h, Msg: err.Error()})
			continue
		}
		c.Close()
		out = append(out, Check{Name: "tcp " + h, OK: true, Msg: addr})
	}
	return out
}

// pick returns the cached host, or settles on one. The dial runs unlocked so
// a slow happyEyeballs doesn't hold up callers with their own contexts.
func (s *SSH) pick(ctx context.Context) (Target, error) {
	s.mu.Lock()
	if s.chosen != nil {
		t := *s.chosen
		s.mu.Unlock()
		return t, nil
	}
	gen := s.gen
	s.mu.Unlock()

	idx := 0
	if len(s.r.Hosts) != 1 {
		var err error
		idx, err = happyEyeballs(ctx, s.r.Hosts, s.r.SSHOptions, 500*time.Millisecond, 5*time.Second)
		if err != nil {
			return Target{}, err
		}
	}
	t := s.target(s.r.Hosts[idx])

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chosen != nil {
		return *s.chosen, nil // a concurrent pick settled first
	}
	if s.gen == gen {
		s.chosen = &t
	}
	return t, nil
}

// resolveSSH asks OpenSSH how it would connect to dest (honors ~/.ssh/config
// aliases, HostName and Port).
func resolveSSH(ctx context.Context, dest string, opts []string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	args := []string{"-G"}
	for _, o := range opts {
		args = append(args, "-o", o)
	}
	out, err := exec.CommandContext(cctx, "ssh", append(args, "--", dest)...).Output()
	if err != nil {
		return "", fmt.Errorf("ssh -G %s: %w", dest, err)
	}
	host, port := "", "22"
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), " ")
		switch k {
		case "hostname":
			host = v
		case "port":
			port = v
		}
	}
	if host == "" {
		return "", fmt.Errorf("ssh -G %s: no hostname", dest)
	}
	return net.JoinHostPort(host, port), nil
}

// happyEyeballs dials every host's SSH port concurrently. The first host wins
// if it connects within grace of the first success; otherwise the
// lowest-index host that connected wins.
func happyEyeballs(ctx context.Context, hosts, opts []string, grace, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type res struct {
		i   int
		err error
	}
	ch := make(chan res, len(hosts))
	for i, h := range hosts {
		go func(i int, h string) {
			addr, err := resolveSSH(ctx, h, opts)
			if err == nil {
				var c net.Conn
				c, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
				if err == nil {
					c.Close()
				}
			}
			ch <- res{i, err}
		}(i, h)
	}
	best := -1
	var graceC <-chan time.Time
	var errs []string
	for n := 0; n < len(hosts); n++ {
		select {
		case r := <-ch:
			if r.err != nil {
				errs = append(errs, hosts[r.i]+": "+r.err.Error())
				continue
			}
			if r.i == 0 {
				return 0, nil
			}
			if best == -1 || r.i < best {
				best = r.i
			}
			if graceC == nil {
				graceC = time.After(grace)
			}
		case <-graceC:
			return best, nil
		case <-ctx.Done():
			if best >= 0 {
				return best, nil
			}
			return -1, fmt.Errorf("no host reachable: %s", strings.Join(errs, "; "))
		}
	}
	if best >= 0 {
		return best, nil
	}
	return -1, fmt.Errorf("no host reachable: %s", strings.Join(errs, "; "))
}

// Itoa helper for providers.
func itoa(i int) string { return strconv.Itoa(i) }
