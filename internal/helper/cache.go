package helper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Shared dependency cache transfer. URLs are presigned S3 URLs from the
// client (the VM holds no AWS credentials), or file:// paths in the Docker
// simulator. Archives are tar with zstd (gzip if zstd is missing); GNU tar
// detects the compression when extracting. Presigned URLs are capabilities:
// they never appear in process arguments, logs or reported errors.

const (
	maxSinglePut   = 5 << 30 // S3 single-PUT limit
	transferBudget = 10 * time.Minute
)

func cacheClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: t}
}

var querySecrets = regexp.MustCompile(`\?[^\s"]*`)

// redact strips query strings (signatures, session tokens) from errors.
func redact(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) {
		msg = fmt.Sprintf("%s %s: %v", ue.Op, stripQuery(ue.URL), ue.Err)
	}
	// Also scrub anything nested in the message (a redirect's URL...).
	return errors.New(querySecrets.ReplaceAllString(msg, "?…"))
}

func stripQuery(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

// cacheRestore downloads and unpacks the archive at url into dst (atomically
// via a temp dir). It returns false, nil on a cache miss.
func cacheRestore(ctx context.Context, root, url, dst string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, transferBudget)
	defer cancel()
	tmpDir := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return false, err
	}
	archive := filepath.Join(tmpDir, fmt.Sprintf("restore-%d.tar", os.Getpid()))
	defer os.Remove(archive)
	ok, err := fetch(ctx, url, archive)
	if err != nil || !ok {
		return false, redact(err)
	}
	stage := fmt.Sprintf("%s.tmp-%d", dst, os.Getpid())
	_ = os.RemoveAll(stage)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return false, err
	}
	// GNU tar strips leading '/', refuses '..' members and defers symlinks
	// that point outside the target, so a member cannot escape stage.
	if out, err := exec.CommandContext(ctx, "tar", "-C", stage, "-xf", archive).CombinedOutput(); err != nil {
		_ = os.RemoveAll(stage)
		return false, fmt.Errorf("unpacking cached dependencies: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if fi, err := os.Lstat(filepath.Join(stage, ".ready")); err != nil || !fi.Mode().IsRegular() {
		_ = os.RemoveAll(stage)
		return false, errors.New("cached archive has no valid .ready marker")
	}

	_ = os.RemoveAll(dst)
	return true, os.Rename(stage, dst)
}

func fetch(ctx context.Context, url, dst string) (bool, error) {
	if path, ok := strings.CutPrefix(url, "file://"); ok {
		in, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		defer in.Close()
		out, err := os.Create(dst)
		if err != nil {
			return false, err
		}
		_, err = io.Copy(out, in)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return err == nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := cacheClient().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 404 && !bodyHas(resp.Body, "NoSuchBucket"):
		return false, nil
	case resp.StatusCode == 403:
		// S3 answers 403 for absent keys when the signer lacks ListBucket.
		return false, nil
	case resp.StatusCode != 200:
		return false, fmt.Errorf("cache download: HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(dst)
	if err != nil {
		return false, err
	}
	_, err = io.Copy(out, resp.Body)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err == nil, err
}

func bodyHas(r io.Reader, s string) bool {
	b, _ := io.ReadAll(io.LimitReader(r, 4096))
	return strings.Contains(string(b), s)
}

// startCacheUpload archives src and uploads it in a detached helper process,
// so the job doesn't wait and the upload outlives this session. The URL is
// passed on stdin, never in argv (visible to every process on the VM).
func startCacheUpload(root, src, lockPath, url string) (*transfer, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logf, _ := os.OpenFile(filepath.Join(root, "cache-upload.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	cmd := exec.Command(self, "cache-put", "--work-root", root, "--src", src, "--lock", lockPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = logf, logf
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	_, _ = io.WriteString(stdin, url+"\n")
	stdin.Close()
	if logf != nil {
		logf.Close()
	}
	return watch(cmd), nil
}

// CachePut is the detached upload (spotexec-helper cache-put; URL on
// stdin). It is bounded in time, holds the lifecycle lock shared so the
// idle watchdog keeps the VM up meanwhile, holds the dependency key's lock
// shared so prune cannot delete the source mid-archive, and gives up if
// the VM starts draining.
func CachePut(root, src, lockPath string) error {
	resetOwnOOMScore() // started by a protected helper: don't keep its score
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("cache-put: no URL on stdin")
	}
	url := strings.TrimSpace(line)
	ctx, cancel := context.WithTimeout(context.Background(), transferBudget)
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			if draining(root) {
				cancel()
				return
			}
			time.Sleep(2 * time.Second)
		}
	}()
	if _, err := os.Stat(LifecycleLock); err == nil {
		if l, err := lockShared(LifecycleLock, 10*time.Second); err == nil {
			defer l.Unlock()
		}
	}
	if lockPath != "" {
		if f, err := openLockFile(lockPath); err == nil {
			defer f.Close()
			// Not a blocking flock: the transfer budget (and a drain)
			// bound the wait too, and a run may be waiting for this upload.
			for syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil {
				select {
				case <-ctx.Done():
					return fmt.Errorf("waiting for %s: %w", lockPath, ctx.Err())
				case <-time.After(200 * time.Millisecond):
				}
			}
		}
	}
	tmpDir := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return err
	}
	archive := filepath.Join(tmpDir, "upload-"+strconv.Itoa(os.Getpid())+".tar")
	defer os.Remove(archive)
	// tar runs the compressor itself and fails if either step fails (a shell
	// pipeline would only report the compressor's status).
	args := []string{"-C", src, "-czf", archive, "."}
	if _, err := exec.LookPath("zstd"); err == nil {
		args = []string{"-C", src, "-I", "zstd -T0 -3 -q", "-cf", archive, "."}
	}
	if out, err := exec.CommandContext(ctx, "tar", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("archiving %s: %v: %s", src, err, out)
	}
	fi, err := os.Stat(archive)
	if err != nil {
		return err
	}
	if fi.Size() > maxSinglePut {
		return fmt.Errorf("dependency archive is %d bytes; over the 5 GiB single-upload limit, not caching", fi.Size())
	}
	start := time.Now()
	if path, ok := strings.CutPrefix(url, "file://"); ok {
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			return err
		}
		tmp := path + ".part" + strconv.Itoa(os.Getpid())
		if out, err := exec.CommandContext(ctx, "cp", archive, tmp).CombinedOutput(); err != nil {
			return fmt.Errorf("cp: %v: %s", err, out)
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	} else {
		f, err := os.Open(archive)
		if err != nil {
			return err
		}
		defer f.Close()
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, f)
		if err != nil {
			return redact(err)
		}
		req.ContentLength = fi.Size()
		resp, err := cacheClient().Do(req)
		if err != nil {
			return redact(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return fmt.Errorf("cache upload: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		}
	}
	fmt.Printf("%s uploaded %d bytes in %s\n", time.Now().Format(time.RFC3339), fi.Size(), time.Since(start).Round(time.Millisecond))
	return nil
}
