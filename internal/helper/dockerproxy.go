package helper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Docker access for project containers ([container] docker = true). The
// command gets a Docker API socket of its own at /var/run/docker.sock: a
// proxy (spotexec-helper docker-proxy) in front of the runner's daemon that
// labels every container, network and volume created through it with the
// slot and the run. The run's cleanup removes what carries its run label,
// and the next holder of the slot removes what carries the slot label, so
// nothing a run started outlives it or touches another slot's. The proxy is
// not a security boundary: Docker access is root-equivalent on the runner.

const (
	labelSlot = "io.spotexec.slot"
	labelRun  = "io.spotexec.run"
)

// createPathRe matches the API calls whose objects are labelled, with or
// without an API version prefix.
var createPathRe = regexp.MustCompile(`^(?:/v[0-9][0-9.]*)?/(containers|networks|volumes)/create$`)

// deletePathRe matches removing a container: its anonymous volumes go with
// it, as with `docker run --rm`, since nothing labelled could find them
// afterwards (an image's VOLUME, such as postgres's data directory, makes
// one on every run).
var deletePathRe = regexp.MustCompile(`^(?:/v[0-9][0-9.]*)?/containers/[^/]+$`)

// prunePathRe matches pruning the kinds of objects the proxy labels. Each
// is scoped to the run's labels, so a run prunes only what it created
// itself, never what a concurrent run in another slot has idle. Images and
// build cache carry no labels of ours and are left as they are.
var prunePathRe = regexp.MustCompile(`^(?:/v[0-9][0-9.]*)?/(containers|networks|volumes)/prune$`)

// dockerSocketPath is the daemon socket the proxy forwards to.
func dockerSocketPath() (string, error) {
	h := os.Getenv("DOCKER_HOST")
	if h == "" {
		return "/var/run/docker.sock", nil
	}
	if p, ok := strings.CutPrefix(h, "unix://"); ok && p != "" {
		return p, nil
	}
	return "", fmt.Errorf("container.docker needs the runner's Docker daemon on a local socket (DOCKER_HOST is %s)", h)
}

// dockerProxyHandler forwards the Docker API to the daemon at upstream and
// labels what is created through it. Nested containers that mount the
// daemon's socket (Testcontainers' Ryuk, a compose service driving docker)
// get sock, the proxy's own socket, instead, so what they create is
// labelled too.
func dockerProxyHandler(upstream, sock string, labels map[string]string) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme, r.Out.URL.Host, r.Out.Host = "http", "docker", "docker"
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", upstream)
			},
			DisableCompression: true,
		},
		FlushInterval: -1, // logs, events and attach stream as they come
		ErrorLog:      log.New(io.Discard, "", 0),
	}
	sockets := []string{"/var/run/docker.sock", "/run/docker.sock", path.Clean(upstream)}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Matched, and forwarded, as the canonical path: the daemon's
		// router cleans a path like //containers/create before routing it,
		// so the raw one would slip past the checks below.
		if !strings.HasPrefix(r.URL.Path, "/") {
			dockerError(w, http.StatusBadRequest, "invalid request path "+strconv.Quote(r.URL.Path))
			return
		}
		r.URL.Path, r.URL.RawPath = path.Clean(r.URL.Path), ""
		if r.Method == http.MethodPost && prunePathRe.MatchString(r.URL.Path) {
			q, err := url.ParseQuery(r.URL.RawQuery)
			if err == nil {
				err = scopePrune(q, labels)
			}
			// The daemon reads a form body's filters ahead of the query's.
			if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && mt == "application/x-www-form-urlencoded" {
				err = errors.New("prune: filters go in the query, not a form body")
			}
			if err != nil {
				dockerError(w, http.StatusBadRequest, err.Error())
				return
			}
			r.URL.RawQuery = q.Encode()
		}
		if m := createPathRe.FindStringSubmatch(r.URL.Path); m != nil && r.Method == http.MethodPost {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
			if err == nil {
				body, err = rewriteCreate(m[1], body, labels, sock, sockets)
			}
			if err != nil {
				dockerError(w, http.StatusBadRequest, err.Error())
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.TransferEncoding = nil
			r.Header.Del("Content-Length")
			r.Header.Del("Transfer-Encoding")
			// The daemon takes a bodyless create, but wants a body typed.
			r.Header.Set("Content-Type", "application/json")
		}
		if r.Method == http.MethodDelete && deletePathRe.MatchString(r.URL.Path) {
			q := r.URL.Query()
			q.Set("v", "1")
			r.URL.RawQuery = q.Encode()
		}
		rp.ServeHTTP(w, r)
	})
}

// scopePrune adds labels to a prune request's filters (q's filters
// parameter), keeping the caller's own: the daemon prunes only what carries
// every label filter, so the caller's filters narrow the run's, never widen
// them. Both encodings the daemon reads ({"label":["k=v"]} and the legacy
// {"label":{"k=v":true}}) are taken; the first is sent.
func scopePrune(q url.Values, labels map[string]string) error {
	if len(labels) == 0 {
		return errors.New("prune: no labels to scope it to")
	}
	if len(q["filters"]) > 1 {
		return errors.New("prune: more than one filters parameter")
	}
	filters := map[string][]string{}
	if raw := q.Get("filters"); raw != "" {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			return fmt.Errorf("invalid filters: %v", err)
		}
		for k, v := range obj {
			var list []string
			if err := json.Unmarshal(v, &list); err != nil {
				// The legacy form: its keys are the values, whatever they map to.
				var set map[string]bool
				if err := json.Unmarshal(v, &set); err != nil {
					return fmt.Errorf("invalid filters: %q is neither a list nor a set of strings", k)
				}
				list = slices.Collect(maps.Keys(set))
			}
			filters[k] = list
		}
	}
	for k, v := range labels {
		filters["label"] = append(filters["label"], k+"="+v)
	}
	for k, v := range filters {
		slices.Sort(v)
		filters[k] = slices.Compact(v)
	}
	b, _ := json.Marshal(filters)
	q.Set("filters", string(b))
	return nil
}

func dockerError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": "spotexec docker proxy: " + msg})
}

// rewriteCreate adds labels to a create request's body (kind is containers,
// networks or volumes), keeping every other field as sent. The daemon
// matches field names case-insensitively, so every spelling of Labels is
// merged into one.
func rewriteCreate(kind string, body []byte, labels map[string]string, sock string, sockets []string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &obj); err != nil {
			return nil, fmt.Errorf("invalid %s create request: %v", kind, err)
		}
	}
	if obj == nil {
		obj = map[string]json.RawMessage{}
	}
	merged := map[string]string{}
	for _, k := range foldKeys(obj, "Labels") {
		if !isNull(obj[k]) {
			var l map[string]string
			if err := json.Unmarshal(obj[k], &l); err != nil {
				return nil, fmt.Errorf("invalid Labels: %v", err)
			}
			maps.Copy(merged, l)
		}
		delete(obj, k)
	}
	maps.Copy(merged, labels)
	obj["Labels"], _ = json.Marshal(merged)
	if kind == "containers" {
		for _, k := range foldKeys(obj, "HostConfig") {
			hc, err := rewriteSocketMounts(obj[k], sock, sockets)
			if err != nil {
				return nil, err
			}
			obj[k] = hc
		}
	}
	return json.Marshal(obj)
}

// rewriteSocketMounts points binds and bind mounts of the daemon's socket
// in a container's HostConfig at sock.
func rewriteSocketMounts(raw json.RawMessage, sock string, sockets []string) (json.RawMessage, error) {
	if isNull(raw) {
		return raw, nil
	}
	var hc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &hc); err != nil {
		return nil, fmt.Errorf("invalid HostConfig: %v", err)
	}
	isSock := func(p string) bool { return p != "" && slices.Contains(sockets, path.Clean(p)) }
	for _, k := range foldKeys(hc, "Binds") {
		if isNull(hc[k]) {
			continue
		}
		var binds []string
		if err := json.Unmarshal(hc[k], &binds); err != nil {
			return nil, fmt.Errorf("invalid HostConfig.Binds: %v", err)
		}
		for i, b := range binds {
			if src, rest, ok := strings.Cut(b, ":"); ok && isSock(src) {
				binds[i] = sock + ":" + rest
			}
		}
		hc[k], _ = json.Marshal(binds)
	}
	for _, k := range foldKeys(hc, "Mounts") {
		if isNull(hc[k]) {
			continue
		}
		var mounts []map[string]json.RawMessage
		if err := json.Unmarshal(hc[k], &mounts); err != nil {
			return nil, fmt.Errorf("invalid HostConfig.Mounts: %v", err)
		}
		for _, m := range mounts {
			var typ string
			for _, tk := range foldKeys(m, "Type") {
				_ = json.Unmarshal(m[tk], &typ)
			}
			if !strings.EqualFold(typ, "bind") {
				continue
			}
			for _, sk := range foldKeys(m, "Source") {
				var src string
				if json.Unmarshal(m[sk], &src) == nil && isSock(src) {
					m[sk], _ = json.Marshal(sock)
				}
			}
		}
		hc[k], _ = json.Marshal(mounts)
	}
	return json.Marshal(hc)
}

// foldKeys returns obj's keys equal to name ignoring case, sorted.
func foldKeys(obj map[string]json.RawMessage, name string) []string {
	var ks []string
	for k := range obj {
		if strings.EqualFold(k, name) {
			ks = append(ks, k)
		}
	}
	slices.Sort(ks)
	return ks
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

// dockerProxyConfig is what the helper sends the proxy on its stdin.
type dockerProxyConfig struct {
	Socket string            `json:"socket"`
	Labels map[string]string `json:"labels"`
}

// DockerProxy is `spotexec-helper docker-proxy`: it reads its config as one
// JSON line on stdin, checks the daemon answers, serves the API on the
// config's socket and prints "ready", then serves until stdin closes (the
// helper stopping it, or dying).
func DockerProxy() error {
	resetOwnOOMScore() // started by a protected helper: don't keep its score
	in := bufio.NewReader(os.Stdin)
	line, err := in.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("docker-proxy: reading config: %w", err)
	}
	var cfg dockerProxyConfig
	if err := json.Unmarshal(line, &cfg); err != nil || cfg.Socket == "" {
		return fmt.Errorf("docker-proxy: bad config: %v", err)
	}
	upstream, err := dockerSocketPath()
	if err != nil {
		return err
	}
	if err := pingDocker(upstream); err != nil {
		return fmt.Errorf("the Docker daemon at %s isn't answering: %w", upstream, err)
	}
	old := syscall.Umask(0o077) // the socket is the runner user's alone
	ln, err := net.Listen("unix", cfg.Socket)
	syscall.Umask(old)
	if err != nil {
		return err
	}
	defer os.Remove(cfg.Socket)
	srv := &http.Server{Handler: dockerProxyHandler(upstream, cfg.Socket, cfg.Labels), ReadHeaderTimeout: time.Minute}
	go func() { _ = srv.Serve(ln) }()
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, in)
	_ = ln.Close()
	return nil
}

func pingDocker(sock string) error {
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	resp, err := c.Get("http://docker/_ping")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/_ping: %s", resp.Status)
	}
	return nil
}

// dockerProxy is a running docker-proxy process.
type dockerProxy struct {
	sock     string
	dir      string
	cmd      *exec.Cmd
	stdin    io.Closer
	out      io.Closer
	done     chan struct{}
	stopOnce sync.Once
}

// proxyDirPrefix starts the directories that hold proxy sockets: short and
// private, as a unix socket path is limited to ~104 bytes, which a path
// under the slot directory can exceed.
const proxyDirPrefix = "/tmp/sx-docker-"

// newProxyDir makes a directory for a proxy's socket.
func newProxyDir() (string, error) {
	return os.MkdirTemp(filepath.Dir(proxyDirPrefix), filepath.Base(proxyDirPrefix))
}

// isProxyDir reports whether dir is a name newProxyDir could have made: a
// direct child of /tmp, so removing it can't follow a symlink elsewhere.
func isProxyDir(dir string) bool {
	return filepath.Dir(dir) == filepath.Dir(proxyDirPrefix) && len(dir) > len(proxyDirPrefix) &&
		strings.HasPrefix(dir, proxyDirPrefix) && !strings.Contains(dir[len(proxyDirPrefix):], "/")
}

// startDockerProxy starts a docker-proxy labelling with labels, serving in
// dir (from newProxyDir, removed by stop), through the same Docker access as
// d (under sg when needed). It carries the workspace marker, so
// reapWorkspace stops it with the run's other processes, and it exits when
// the helper does (its stdin closes), sg or not.
func startDockerProxy(d dockerRunner, dir, workspace string, labels map[string]string) (*dockerProxy, error) {
	self, err := os.Executable()
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	p := &dockerProxy{sock: filepath.Join(dir, "docker.sock"), dir: dir, done: make(chan struct{})}
	if d.viaSG {
		p.cmd = exec.Command("sg", "docker", "-c", "exec "+shq(self)+" docker-proxy")
	} else {
		p.cmd = exec.Command(self, "docker-proxy")
	}
	p.cmd.SysProcAttr = childSysProcAttr()
	p.cmd.Env = append(os.Environ(), "SPOTEXEC_WORKSPACE="+workspace)
	stdin, err := p.cmd.StdinPipe()
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	p.cmd.Stdout, p.cmd.Stderr = pw, pw
	if err := p.cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	pw.Close()
	p.stdin, p.out = stdin, pr
	go func() { _ = p.cmd.Wait(); close(p.done) }()
	b, _ := json.Marshal(dockerProxyConfig{Socket: p.sock, Labels: labels})
	_, _ = stdin.Write(append(b, '\n'))
	first := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(pr).ReadString('\n')
		first <- strings.TrimSpace(line)
	}()
	select {
	case line := <-first:
		if line == "ready" {
			return p, nil
		}
		p.stop()
		if line == "" {
			line = "it exited"
		}
		return nil, fmt.Errorf("starting the Docker socket for the container: %s", strings.TrimPrefix(line, "spotexec-helper: "))
	case <-time.After(30 * time.Second):
		p.stop()
		return nil, errors.New("starting the Docker socket for the container: timed out")
	}
}

// stop stops the proxy (no new API calls from the run) and removes its
// socket. It is safe to call more than once.
func (p *dockerProxy) stop() {
	p.stopOnce.Do(func() {
		p.stdin.Close()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
			select {
			case <-p.done:
			case <-time.After(5 * time.Second):
			}
		}
		p.out.Close()
		os.RemoveAll(p.dir)
	})
}

// removeLabelled force-removes the containers (with their anonymous
// volumes), then the networks, then the volumes labelled label (key=value),
// and returns what is still there ("" if nothing). They are listed and
// removed by ID (a volume's name is its ID), which xargs can't split or
// misquote the way it could a network's name.
func removeLabelled(ctx context.Context, d dockerRunner, label string) string {
	f := "--filter " + shq("label="+label)
	lists := []struct{ kind, list, rm string }{
		{"container", "ps -aq --no-trunc " + f, "rm -fv"},
		{"network", "network ls -q --no-trunc " + f, "network rm"},
		{"volume", "volume ls -q " + f, "volume rm -f"},
	}
	var left []string
	for attempt := 0; attempt < 3; attempt++ {
		left = left[:0]
		for _, l := range lists {
			_, _ = runShell(ctx, d.cmd(l.list+" | xargs -r docker "+l.rm)+" >/dev/null 2>&1")
		}
		for _, l := range lists {
			out, err := runShell(ctx, d.cmd(l.list))
			if err != nil {
				left = append(left, "(docker "+strings.Fields(l.list)[0]+" failed: "+firstLines(string(out), 2)+")")
				continue
			}
			for _, id := range strings.Fields(string(out)) {
				left = append(left, l.kind+" "+id)
			}
		}
		if len(left) == 0 || ctx.Err() != nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	return strings.Join(left, ", ")
}
