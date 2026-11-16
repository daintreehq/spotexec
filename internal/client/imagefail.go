package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/transport"
	"github.com/daintreehq/spotexec/internal/ui"
)

// Remembering a failed image build (#194). A project image's tag hashes its
// Dockerfile, build context and platform, so a build that failed on one
// runner fails the same way on the next. The share lock only stops the runs
// waiting on a build at the same moment from building it at once; this
// record stops them, and every later run on this machine in any region,
// from building it at all: they fail at once with the original build's
// error and output. It is forgotten when the image is had after all, when
// it ages out, or when `spotexec container build` builds it again. A build
// lost with its runner or cancelled is never recorded: the helper marks
// only the build itself exiting non-zero.

// imageFailAge is how long a failed build is remembered: long enough for
// the parts of a sharded or parallel task and the runs that follow it, short
// enough that a build broken by a passing network or registry fault is
// tried again without being asked.
var imageFailAge = time.Hour

func imageFailDir() string { return filepath.Join(config.StateDir(), "share", "image-failed") }
func imageLogDir() string  { return filepath.Join(config.StateDir(), "logs", "image") }

// imageMemo remembers, on this machine, the failed builds of one
// repository's images ("" dir: remembers nothing).
type imageMemo struct {
	dir, repo string
	now       func() time.Time // nil: time.Now
}

func newImageMemo(repo string) imageMemo { return imageMemo{dir: imageFailDir(), repo: repo} }

// imageFailure is one remembered failed build.
type imageFailure struct {
	Repo      string    `json:"repo"`
	Tag       string    `json:"tag"`
	Code      string    `json:"code,omitempty"` // the helper's error code ("": IMAGE_FAILED)
	Message   string    `json:"message"`        // the helper's error
	Who       string    `json:"who"`            // the run that built it: "aws#3"
	At        time.Time `json:"at"`
	Tail      string    `json:"tail,omitempty"` // its last lines of output
	Truncated bool      `json:"truncated,omitempty"`
	Log       string    `json:"log,omitempty"` // its full output, on this machine
}

func (m imageMemo) on(tag string) bool { return m.dir != "" && tag != "" }

func (m imageMemo) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m imageMemo) path(tag string) string {
	h := sha256.Sum256([]byte(m.repo + "\x00" + tag))
	return filepath.Join(m.dir, hex.EncodeToString(h[:])[:32]+".json")
}

// recall returns the tag's remembered failure, or nil when there is none
// or it has aged out.
func (m imageMemo) recall(tag string) *imageFailure {
	if !m.on(tag) {
		return nil
	}
	b, err := os.ReadFile(m.path(tag))
	if err != nil {
		return nil
	}
	var f imageFailure
	if json.Unmarshal(b, &f) != nil || f.Repo != m.repo || f.Tag != tag {
		return nil
	}
	if !m.clock().Before(f.At.Add(imageFailAge)) {
		return nil
	}
	return &f
}

// remember records a failed build, and drops records that have aged out.
func (m imageMemo) remember(f imageFailure) {
	if !m.on(f.Tag) {
		return
	}
	f.Repo = m.repo
	b, err := json.Marshal(f)
	if err != nil || os.MkdirAll(m.dir, 0o700) != nil {
		return
	}
	_ = writeAtomic(m.path(f.Tag), b)
	m.prune()
}

// forget drops the tag's record: the image was had after all.
func (m imageMemo) forget(tag string) {
	if m.on(tag) {
		_ = os.Remove(m.path(tag))
	}
}

// prune removes the records (of any repository) that have aged out, and
// any that can't be read.
func (m imageMemo) prune() {
	ents, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	now := m.clock()
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(m.dir, e.Name())
		var f imageFailure
		if b, err := os.ReadFile(p); err == nil && (json.Unmarshal(b, &f) != nil || !now.Before(f.At.Add(imageFailAge))) {
			_ = os.Remove(p)
		}
	}
}

// ImageBuildError is a failed build of the project's image: this run's own
// build, or (Cached) one an earlier run made, which this run didn't repeat.
// Its message stays one line; Detail explains it. When the project's own
// Dockerfile is at fault (Project), the run exits ExitImageBuild rather
// than ExitInfra and is neither replayed nor run locally instead (#193).
type ImageBuildError struct {
	Err    error
	Cached bool
	Until  time.Time // builds of this image fail at once until then (zero: not remembered)
	imageFailure
}

func (e *ImageBuildError) Error() string { return e.Err.Error() }
func (e *ImageBuildError) Unwrap() error { return e.Err }

// Project reports whether the project's Dockerfile itself failed to build
// (IMAGE_BUILD_FAILED): the project's failure, not spotexec's.
func (e *ImageBuildError) Project() bool { return e.Code == protocol.CodeImageBuildFailed }

// cachedError is the error of a run that needs an image whose build f
// failed: the original build's, marked as not repeated.
func (f imageFailure) cachedError(now time.Time) *ImageBuildError {
	msg := fmt.Sprintf("%s (from %s's build %s ago; not building it again)", f.Message, f.Who, ui.Dur(now.Sub(f.At).Round(time.Second)))
	code := f.Code
	if code == "" {
		code = protocol.CodeImageFailed
	}
	re := &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: code, Message: msg, BuildFailed: true}}
	return &ImageBuildError{Err: fmt.Errorf("container image: %w", re), Cached: true, Until: f.At.Add(imageFailAge), imageFailure: f}
}

// Detail is what to show under the error: the build's output (a fresh
// build's was shown as it ran, but the end of it is shown again when the
// Dockerfile is at fault), where its full log is, and how to build the
// image again.
func (e *ImageBuildError) Detail() []string {
	var out []string
	if (e.Cached || e.Project()) && e.Tail != "" {
		lines := ui.CleanLines(e.Tail)
		switch {
		case e.Truncated:
			out = append(out, fmt.Sprintf("last %d lines of the build's output:", len(lines)))
		case e.Cached:
			out = append(out, "the build's output:")
		default:
			out = append(out, "end of the build's output:")
		}
		for _, l := range lines {
			out = append(out, "  "+l)
		}
	}
	if e.Log != "" {
		if _, err := os.Stat(e.Log); err == nil {
			out = append(out, "full log: "+e.Log)
		}
	}
	if !e.Until.IsZero() {
		out = append(out, fmt.Sprintf("runs needing this image fail at once until %s, unless its Dockerfile or build context changes; `spotexec container build` builds it again now",
			e.Until.Local().Format("15:04")))
	}
	return out
}
