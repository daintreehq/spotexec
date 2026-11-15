package helper

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// The test binary stands in for spotexec-helper when startDockerProxy runs
// os.Executable() docker-proxy.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "docker-proxy" {
		if err := DockerProxy(); err != nil {
			fmt.Fprintln(os.Stderr, "spotexec-helper:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

var testLabels = map[string]string{labelSlot: "sx-repo-1-", labelRun: "sx-repo-1-abc"}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return m
}

// Labels are added to what the caller set (whatever its spelling); the
// reserved ones can't be overridden, and every other field is kept.
func TestRewriteCreateLabels(t *testing.T) {
	body := `{"Image":"postgres:16","labels":{"a":"1"},"Labels":{"b":"2","io.spotexec.run":"other"},"Env":["X=1"]}`
	out, err := rewriteCreate("containers", []byte(body), testLabels, "/tmp/p.sock", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	if _, ok := m["labels"]; ok {
		t.Fatal("lower-case labels kept beside Labels")
	}
	l := m["Labels"].(map[string]any)
	want := map[string]string{"a": "1", "b": "2", labelSlot: "sx-repo-1-", labelRun: "sx-repo-1-abc"}
	if len(l) != len(want) {
		t.Fatalf("labels = %v", l)
	}
	for k, v := range want {
		if l[k] != v {
			t.Fatalf("labels = %v, want %v", l, want)
		}
	}
	if m["Image"] != "postgres:16" || m["Env"].([]any)[0] != "X=1" {
		t.Fatalf("fields lost: %s", out)
	}
	for _, b := range []string{``, `null`, `{"Name":"n","Labels":null}`} {
		out, err := rewriteCreate("volumes", []byte(b), testLabels, "", nil)
		if err != nil {
			t.Fatalf("%q: %v", b, err)
		}
		if l := decode(t, out)["Labels"].(map[string]any); l[labelRun] != "sx-repo-1-abc" {
			t.Fatalf("%q: %s", b, out)
		}
	}
	for _, b := range []string{`{`, `[]`, `{"Labels":["a"]}`, `{"HostConfig":{"Binds":"x"}}`} {
		if _, err := rewriteCreate("containers", []byte(b), testLabels, "", nil); err == nil {
			t.Fatalf("%q accepted", b)
		}
	}
}

// A nested container that mounts the daemon's socket gets the proxy's.
func TestRewriteCreateSocketMounts(t *testing.T) {
	body := `{"HostConfig":{"Binds":["/var/run/docker.sock:/var/run/docker.sock:rw","/run/docker.sock:/d.sock","/data:/data","vol:/v"],
		"Mounts":[{"Type":"bind","Source":"/var/run//docker.sock","Target":"/s"},{"Type":"volume","Source":"/var/run/docker.sock","Target":"/v"},{"Type":"bind","Source":"/srv","Target":"/srv"}],
		"NetworkMode":"host"}}`
	out, err := rewriteCreate("containers", []byte(body), testLabels, "/tmp/sx/docker.sock", []string{"/var/run/docker.sock", "/run/docker.sock"})
	if err != nil {
		t.Fatal(err)
	}
	hc := decode(t, out)["HostConfig"].(map[string]any)
	binds := fmt.Sprint(hc["Binds"])
	if binds != "[/tmp/sx/docker.sock:/var/run/docker.sock:rw /tmp/sx/docker.sock:/d.sock /data:/data vol:/v]" {
		t.Fatalf("binds = %s", binds)
	}
	mounts := hc["Mounts"].([]any)
	if src := mounts[0].(map[string]any)["Source"]; src != "/tmp/sx/docker.sock" {
		t.Fatalf("bind mount source = %v", src)
	}
	if src := mounts[1].(map[string]any)["Source"]; src != "/var/run/docker.sock" {
		t.Fatalf("volume mount rewritten: %v", src)
	}
	if src := mounts[2].(map[string]any)["Source"]; src != "/srv" {
		t.Fatalf("other bind rewritten: %v", src)
	}
	if hc["NetworkMode"] != "host" {
		t.Fatal("HostConfig field lost")
	}
}

// fakeDaemon serves a Docker-like API on a unix socket: it records create
// bodies, streams /events until the test ends, and echoes an upgraded
// attach until the client half-closes, then says "bye".
type fakeDaemon struct {
	sock    string
	created chan string
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sx-fake-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeDaemon{sock: filepath.Join(dir, "d.sock"), created: make(chan string, 10)}
	ln, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/_ping", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "OK") })
	create := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.ContentLength != int64(len(b)) {
			http.Error(w, "content length mismatch", 400)
			return
		}
		f.created <- r.URL.Path + "?" + r.URL.RawQuery + " " + string(b)
		w.WriteHeader(201)
		io.WriteString(w, `{"Id":"x"}`)
	}
	mux.HandleFunc("POST /v1.45/containers/create", create)
	mux.HandleFunc("POST /networks/create", create)
	mux.HandleFunc("POST /volumes/create", func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > 0 && r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "unsupported Content-Type", 400)
			return
		}
		create(w, r)
	})
	mux.HandleFunc("DELETE /containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.created <- "DELETE " + r.URL.Path + "?" + r.URL.RawQuery
		w.WriteHeader(204)
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"start"}`+"\n")
		w.(http.Flusher).Flush()
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/containers/x/attach", func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		brw.Flush()
		got, _ := io.ReadAll(brw) // until the client's half-close
		conn.Write(append(got, []byte(" bye")...))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { close(stop); srv.Close() })
	return f
}

func unixClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
}

// serveProxy serves dockerProxyHandler in-process on a socket of its own.
func serveProxy(t *testing.T, upstream string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sx-proxy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "p.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: dockerProxyHandler(upstream, sock, testLabels)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func TestDockerProxyLabelsCreates(t *testing.T) {
	f := newFakeDaemon(t)
	c := unixClient(serveProxy(t, f.sock))
	// Versioned, with a name query; and chunked (no Content-Length).
	resp, err := c.Post("http://docker/v1.45/containers/create?name=pg", "application/json", strings.NewReader(`{"Image":"postgres"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	got := <-f.created
	if !strings.HasPrefix(got, "/v1.45/containers/create?name=pg ") || !strings.Contains(got, `"io.spotexec.run":"sx-repo-1-abc"`) || !strings.Contains(got, `"Image":"postgres"`) {
		t.Fatalf("daemon got %s", got)
	}
	pr, pw := io.Pipe()
	go func() { pw.Write([]byte(`{"Name":"net1"}`)); pw.Close() }()
	req, _ := http.NewRequest("POST", "http://docker/networks/create", pr)
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := <-f.created; !strings.Contains(got, `"io.spotexec.slot":"sx-repo-1-"`) || !strings.Contains(got, `"Name":"net1"`) {
		t.Fatalf("daemon got %s", got)
	}
	// A bodyless volume create gets a body, typed as the daemon wants it.
	resp, err = c.Post("http://docker/volumes/create", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("bodyless volume create: status %d", resp.StatusCode)
	}
	if got := <-f.created; !strings.Contains(got, labelRun) {
		t.Fatalf("daemon got %s", got)
	}
	// Removing a container takes its anonymous volumes with it.
	req, _ = http.NewRequest("DELETE", "http://docker/containers/pg?force=1", nil)
	if resp, err = c.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := <-f.created; got != "DELETE /containers/pg?force=1&v=1" {
		t.Fatalf("daemon got %s", got)
	}
	// A body the daemon couldn't parse either is refused, Docker-style.
	resp, err = c.Post("http://docker/networks/create", "application/json", strings.NewReader(`{`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), `"message"`) {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
}

// Streams reach the caller as they come, before the daemon ends them.
func TestDockerProxyStreams(t *testing.T) {
	f := newFakeDaemon(t)
	c := unixClient(serveProxy(t, f.sock))
	resp, err := c.Get("http://docker/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- l
	}()
	select {
	case l := <-line:
		if !strings.Contains(l, "start") {
			t.Fatalf("got %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event not flushed through the proxy")
	}
}

// An upgraded attach is passed through both ways, half-close included
// (`echo x | docker run -i image cat` waits for output after its stdin ends).
func TestDockerProxyUpgrade(t *testing.T) {
	f := newFakeDaemon(t)
	conn, err := net.Dial("unix", serveProxy(t, f.sock))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(conn, "POST /containers/x/attach?stream=1&stdin=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 101 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	io.WriteString(conn, "hello")
	conn.(*net.UnixConn).CloseWrite()
	got, _ := io.ReadAll(br)
	if string(got) != "hello bye" {
		t.Fatalf("got %q", got)
	}
}

// The proxy process: ready once the daemon answers, serving on a private
// socket, and gone (socket and all) once stopped.
func TestStartDockerProxy(t *testing.T) {
	f := newFakeDaemon(t)
	t.Setenv("DOCKER_HOST", "unix://"+f.sock)
	dir, err := newProxyDir()
	if err != nil {
		t.Fatal(err)
	}
	p, err := startDockerProxy(dockerRunner{}, dir, t.TempDir(), testLabels)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p.sock)
	if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("socket %v: %v", fi, err)
	}
	resp, err := unixClient(p.sock).Post("http://docker/networks/create", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := <-f.created; !strings.Contains(got, labelRun) {
		t.Fatalf("daemon got %s", got)
	}
	p.stop()
	p.stop()
	if _, err := os.Stat(p.dir); !os.IsNotExist(err) {
		t.Fatal("socket directory left behind")
	}
	select {
	case <-p.done:
	default:
		t.Fatal("proxy still running")
	}
}

func TestStartDockerProxyNoDaemon(t *testing.T) {
	for host, want := range map[string]string{"unix:///nonexistent/docker.sock": "isn't answering", "tcp://10.0.0.1:2375": "local socket"} {
		t.Setenv("DOCKER_HOST", host)
		dir, err := newProxyDir()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := startDockerProxy(dockerRunner{}, dir, t.TempDir(), testLabels); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: got %v", host, err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s: socket directory left behind", host)
		}
	}
}

// statefulDocker is a docker that works and holds the containers, networks
// and volumes named in its state files (ps, network, volume): listing one
// by label prints its file, removing it (rm -fv, network rm, volume rm)
// empties the file, unless stuck. Every call is logged, one per line.
func statefulDocker(t *testing.T, stuck bool, ps, network, volume string) (bin, log string) {
	t.Helper()
	bin = t.TempDir()
	log = filepath.Join(bin, "calls")
	for f, v := range map[string]string{"ps": ps, "network": network, "volume": volume} {
		if err := os.WriteFile(filepath.Join(bin, f), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rm := `: > "$d/$1"`
	if stuck {
		rm = "true"
	}
	script := `#!/bin/sh
d=` + shq(bin) + `
echo "$*" >> "$d/calls"
case "$1 $2" in
"ps "*) case "$*" in *label=*) cat "$d/ps" ;; esac ;;
"rm -fv") set -- ps; ` + rm + ` ;;
"network ls"|"volume ls") cat "$d/$1" ;;
"network rm"|"volume rm") ` + rm + ` ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// callsWith returns the logged calls containing every one of parts.
func callsWith(t *testing.T, log string, parts ...string) []string {
	t.Helper()
	b, _ := os.ReadFile(log)
	var got []string
	for _, l := range strings.Split(string(b), "\n") {
		ok := l != ""
		for _, p := range parts {
			ok = ok && strings.Contains(l, p)
		}
		if ok {
			got = append(got, l)
		}
	}
	return got
}

// A container.docker run gets the proxy's socket and the environment that
// points at it; its cleanup removes what carries its run label, gives the
// workspace's files back and clears its markers. A run without it is as
// before.
func TestContainerScriptDocker(t *testing.T) {
	f := newFakeDaemon(t)
	t.Setenv("DOCKER_HOST", "unix://"+f.sock)
	bin, calls := statefulDocker(t, false, "", "", "")
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	slot := t.TempDir()
	s := &Session{slotDir: slot, workspace: filepath.Join(slot, "workspace"), repoID: "repo"}
	if err := os.MkdirAll(s.workspace, 0o755); err != nil {
		t.Fatal(err)
	}

	host, _, cleanup, err := s.containerScript(&protocol.ContainerRun{Image: "node:22"}, "true", s.workspace, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(host, "docker.sock") {
		t.Fatalf("socket without container.docker: %s", host)
	}
	envs, _ := filepath.Glob(filepath.Join(slot, "run", "*.env"))
	if env, _ := os.ReadFile(envs[0]); strings.Contains(string(env), "DOCKER_HOST") {
		t.Fatalf("docker environment without container.docker:\n%s", env)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{rootRunMarker, dockerRunMarker} {
		if _, err := os.Stat(filepath.Join(slot, m)); !os.IsNotExist(err) {
			t.Fatalf("%s written without container.docker", m)
		}
	}

	host, _, cleanup, err = s.containerScript(&protocol.ContainerRun{Image: "node:22", Docker: true}, "true", s.workspace, map[string]string{"TESTCONTAINERS_HOST_OVERRIDE": "10.1.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	name := s.lastContainer.name
	if !strings.Contains(host, ",target=/var/run/docker.sock'") {
		t.Fatalf("no socket mount: %s", host)
	}
	envs, _ = filepath.Glob(filepath.Join(slot, "run", "*.env"))
	if len(envs) != 1 {
		t.Fatalf("env files %v", envs)
	}
	env, _ := os.ReadFile(envs[0])
	for _, want := range []string{"DOCKER_HOST=unix:///var/run/docker.sock\n", "TESTCONTAINERS_HOST_OVERRIDE=10.1.1.1\n", "COMPOSE_PROJECT_NAME=" + strings.Trim(s.containerPrefix(), "-") + "\n"} {
		if !strings.Contains(string(env), want) {
			t.Fatalf("env file lacks %q:\n%s", want, env)
		}
	}
	for _, m := range []string{rootRunMarker, dockerRunMarker} {
		if _, err := os.Stat(filepath.Join(slot, m)); err != nil {
			t.Fatalf("%s not written: %v", m, err)
		}
	}
	pdir := s.nestedProxyDir(t, name)
	if b, _ := os.ReadFile(filepath.Join(slot, dockerRunMarker)); strings.TrimSpace(string(b)) != pdir {
		t.Fatalf("marker %q doesn't name the socket directory %s", b, pdir)
	}
	// What the run started is there when it ends.
	for f, v := range map[string]string{"ps": "pg\n", "network": "net\n", "volume": "vol\n"} {
		os.WriteFile(filepath.Join(bin, f), []byte(v), 0o644)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	for _, rm := range []string{"rm -fv pg", "network rm net", "volume rm -f vol"} {
		if len(callsWith(t, calls, rm)) != 1 {
			t.Fatalf("no %q:\n%s", rm, callsWith(t, calls))
		}
	}
	for _, list := range []string{"ps -a --format", "network ls", "volume ls"} {
		if len(callsWith(t, calls, list, "label="+labelRun+"="+name)) == 0 {
			t.Fatalf("%s not filtered by the run's label", list)
		}
	}
	for _, m := range []string{rootRunMarker, dockerRunMarker} {
		if _, err := os.Stat(filepath.Join(slot, m)); !os.IsNotExist(err) {
			t.Fatalf("%s kept after cleanup", m)
		}
	}
	if _, err := os.Stat(pdir); !os.IsNotExist(err) {
		t.Fatal("socket directory left behind")
	}
	if _, ok := s.nested.Load(name); ok {
		t.Fatal("run still tracked")
	}
}

// nestedProxyDir is the socket directory of the tracked run name.
func (s *Session) nestedProxyDir(t *testing.T, name string) string {
	t.Helper()
	v, ok := s.nested.Load(name)
	if !ok {
		t.Fatal("run not tracked")
	}
	return v.(nestedRun).proxy.dir
}

// What the run started that can't be removed keeps the results from being
// collected, and the run tracked and marked for the slot's next holder.
func TestContainerScriptDockerCleanupFails(t *testing.T) {
	f := newFakeDaemon(t)
	t.Setenv("DOCKER_HOST", "unix://"+f.sock)
	bin, _ := statefulDocker(t, true, "", "", "")
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	slot := t.TempDir()
	s := &Session{slotDir: slot, workspace: filepath.Join(slot, "workspace"), repoID: "repo"}
	os.MkdirAll(s.workspace, 0o755)
	_, _, cleanup, err := s.containerScript(&protocol.ContainerRun{Image: "node:22", Docker: true}, "true", s.workspace, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	name := s.lastContainer.name
	os.WriteFile(filepath.Join(bin, "network"), []byte("net\n"), 0o644)
	if err := cleanup(); err == nil || !strings.Contains(err.Error(), "net") {
		t.Fatalf("got %v", err)
	}
	if _, ok := s.nested.Load(name); !ok {
		t.Fatal("run no longer tracked")
	}
	for _, m := range []string{rootRunMarker, dockerRunMarker} {
		if _, err := os.Stat(filepath.Join(slot, m)); err != nil {
			t.Fatalf("%s dropped: %v", m, err)
		}
	}
}

func TestIsProxyDir(t *testing.T) {
	for dir, ok := range map[string]bool{
		"/tmp/sx-docker-123":         true,
		"/tmp/sx-docker-":            false,
		"/tmp/sx-docker-1/link/data": false,
		"/tmp/sx-docker-1/..":        false,
		"/tmp/other":                 false,
		"/var/tmp/sx-docker-1":       false,
		"/tmp/sx-docker-1/../../etc": false,
	} {
		if isProxyDir(dir) != ok {
			t.Errorf("isProxyDir(%q) = %v", dir, !ok)
		}
	}
}

// The next holder of a slot removes what an aborted container.docker run
// left, by the slot's label, and its proxy's socket directory, only when
// the run's marker says there may be some.
func TestSweepSlotContainersDockerRun(t *testing.T) {
	bin, calls := statefulDocker(t, false, "", "net\n", "vol\n")
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	slot := t.TempDir()
	s := &Session{slotDir: slot, repoID: "repo"}
	if left := s.sweepSlotContainers(context.Background(), dockerRunner{}); left != "" {
		t.Fatal(left)
	}
	if got := callsWith(t, calls, "label="); len(got) > 0 {
		t.Fatalf("label sweep without the marker: %v", got)
	}
	pdir, err := newProxyDir()
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(slot, dockerRunMarker), []byte(pdir+"\n"+outside+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if left := s.sweepSlotContainers(context.Background(), dockerRunner{}); left != "" {
		t.Fatal(left)
	}
	for _, list := range []string{"ps -a --format", "network ls", "volume ls"} {
		if len(callsWith(t, calls, list, "label="+labelSlot+"="+s.containerPrefix())) == 0 {
			t.Fatalf("%s not swept by the slot's label", list)
		}
	}
	for _, rm := range []string{"network rm net", "volume rm -f vol"} {
		if len(callsWith(t, calls, rm)) != 1 {
			t.Fatalf("no %q", rm)
		}
	}
	if _, err := os.Stat(pdir); !os.IsNotExist(err) {
		t.Fatal("socket directory left behind")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("a directory that isn't a socket directory was removed")
	}
	if _, err := os.Stat(filepath.Join(slot, dockerRunMarker)); !os.IsNotExist(err) {
		t.Fatal("marker kept after a clean sweep")
	}
}
