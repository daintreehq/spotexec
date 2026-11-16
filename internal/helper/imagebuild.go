package helper

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// buildTailLines is how much of a failed build's output its error carries:
// enough for BuildKit's closing summary of the failing step.
const buildTailLines = 40

// buildLineMax caps a line held for the tail, and an unfinished line held
// while more of it comes.
const buildLineMax = 4 << 10

// buildOverlap is how much of an endless line's end, once classified, is
// classified again with what follows, so no diagnostic straddles a cut.
const buildOverlap = 512

// Evidence that the project's Dockerfile itself is broken (#193). Docker's
// build output is not a typed API, so only these well-known diagnostics
// count; anything else stays a failure of spotexec's own (exit 125).
// They count only on Docker's own lines, never on a line a step printed.
var (
	// BuildKit: a RUN step that exited non-zero ("process "/bin/sh -c …"
	// did not complete successfully: exit code: 2"; before BuildKit 0.11,
	// "executor failed running [/bin/sh -c …]: exit code: 2").
	buildRunExitRe = regexp.MustCompile(`(?:did not complete successfully|executor failed running \[.*\]): exit code: (\d+)`)
	// The legacy builder's form of the same.
	legacyRunExitRe = regexp.MustCompile(`returned a non-zero code: (\d+)`)
	// A Dockerfile that doesn't parse ("dockerfile parse error on line 3:
	// unknown instruction: RUNN"; the legacy builder's "Dockerfile parse
	// error line 3: …").
	parseErrorRe = regexp.MustCompile(`(?i)dockerfile parse error`)
	// COPY or ADD of a file that isn't in the build context: BuildKit (and
	// before 0.11, its "failed to compute cache key: "/x" not found"), then
	// the legacy builder (and its older "COPY failed: stat …: no such file").
	missingSourceRe = regexp.MustCompile(`failed to calculate checksum of ref [^ ]+ "[^"]*": not found|failed to compute cache key: "[^"]*" not found|(?:COPY|ADD) failed: (?:file not found in build context|stat [^:]*: no such file or directory)`)
	// A line a BuildKit step printed ("#5 0.214 …"), as it ran or repeated
	// in the failure's summary ("0.214 …"): the step's words, not Docker's.
	stepLineRe = regexp.MustCompile(`^(?:#\d+ )?\d+\.\d+ `)
	// A line quoting the Dockerfile: a step's heading ("#5 [2/2] RUN …",
	// " > [2/2] RUN …:", the legacy "Step 2/2 : RUN …") or the summary's
	// excerpt ("   2 | >>> RUN …"). It's no evidence of anything.
	dockerfileLineRe = regexp.MustCompile(`^(?:#\d+ | > )\[(?:[^\]]* )?\d+/\d+\] [A-Z]+ |^Step \d+/\d+ : [A-Z]+ |^\s+\d+ \| (?:>>> |    )`)
	// The RUN command Docker quotes in its diagnostics, which is the
	// Dockerfile's text and no evidence either ("RUN grep ECONNRESET log").
	quotedCommands = []struct {
		re   *regexp.Regexp
		repl string
	}{
		{regexp.MustCompile(`process ".*?" did not complete successfully: exit code:`), `process "…" did not complete successfully: exit code:`},
		{regexp.MustCompile(`executor failed running \[.*?\]: exit code:`), `executor failed running […]: exit code:`},
		{regexp.MustCompile(`The command '.*?' returned a non-zero code:`), `The command '…' returned a non-zero code:`},
	}
	// Evidence of Docker, the network or the machine failing, anywhere in
	// the output, which wins over the above: a RUN step that fails because
	// DNS is down, or a server answered 503, is not the Dockerfile's fault.
	buildInfraRe = regexp.MustCompile(`(?i)cannot connect to the docker daemon|error during connect|rpc error: code = unavailable|no space left on device|input/output error|` +
		`temporary failure in name resolution|temporary failure resolving|could not resolve host|could not resolve '|could not connect to|unable to resolve host address|no such host|network is unreachable|` +
		`i/o timeout|tls handshake timeout|connection reset by peer|connection refused|connection timed out|operation timed out|read timed out|` +
		`failed to connect to|max retries exceeded|curl: \((?:5|6|7|28|35|52|56)\)|returned error: (?:429|5\d\d)\b|` +
		`\b(?:429 too many requests|502 bad gateway|503 service unavailable|504 gateway time-?out)\b|toomanyrequests|` +
		`\b(?:EAI_AGAIN|ETIMEDOUT|ECONNRESET|ECONNREFUSED|ENOTFOUND)\b`)
)

// buildLog watches a docker build's output as it streams to the client:
// it keeps the last lines for the error, and what they say about why the
// build failed. Every line is classified as it arrives, so a long build
// can't push early evidence out of the tail.
type buildLog struct {
	mu      sync.Mutex
	partial [2][]byte // per stream, a line not yet ended
	long    [2]bool   // that line outgrew buildLineMax: only its end is held
	tail    []string
	project string // why the Dockerfile is at fault, if it is
	infra   bool   // the output shows Docker, the network or the VM failing
}

// write takes a chunk of stream i's output (0 stdout, 1 stderr).
func (b *buildLog) write(i int, p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	buf := append(b.partial[i], p...)
	for {
		n := bytes.IndexByte(buf, '\n')
		if n < 0 {
			break
		}
		b.line(string(buf[:n]), b.long[i])
		b.long[i] = false
		buf = buf[n+1:]
	}
	if len(buf) > buildLineMax {
		// An endless line (a progress bar): keep its end only, once what
		// is dropped has been looked at for a failure around the build.
		cut := len(buf) - buildLineMax
		if buildInfraRe.Match(buf[:cut+buildOverlap]) {
			b.infra = true
		}
		buf = buf[cut:]
		b.long[i] = true
	}
	b.partial[i] = append([]byte(nil), buf...)
}

// line classifies one line and adds it to the tail. A long line (whose
// start was dropped) can still show a failure around the build, but can't
// show the Dockerfile at fault: its start would say whether a step printed
// it.
func (b *buildLog) line(l string, long bool) {
	l = strings.TrimRight(l, "\r")
	// Where the line came from is decided on all of it: a progress bar's
	// rewrites (\r) could otherwise hide that a step printed it.
	if !dockerfileLineRe.MatchString(l) {
		step := stepLineRe.MatchString(l)
		bare := l
		if !step {
			for _, q := range quotedCommands {
				bare = q.re.ReplaceAllLiteralString(bare, q.repl)
			}
		}
		if buildInfraRe.MatchString(bare) {
			b.infra = true
		}
		if b.project == "" && !long && !step {
			b.project = projectCause(bare)
		}
	}
	if j := strings.LastIndexByte(l, '\r'); j >= 0 {
		l = l[j+1:] // a progress bar's last state
	}
	if len(l) > buildLineMax {
		l = l[len(l)-buildLineMax:]
	}
	b.tail = append(b.tail, l)
	if len(b.tail) > buildTailLines {
		b.tail = b.tail[len(b.tail)-buildTailLines:]
	}
}

// projectCause says how line l shows the Dockerfile at fault, or "".
func projectCause(l string) string {
	for _, re := range []*regexp.Regexp{buildRunExitRe, legacyRunExitRe} {
		if m := re.FindStringSubmatch(l); m != nil {
			code, _ := strconv.Atoi(m[1])
			if code == 137 || code == 143 {
				// Killed (out of memory, or the VM stopping it): not
				// something the step did.
				return ""
			}
			return "a RUN step exited " + m[1]
		}
	}
	if parseErrorRe.MatchString(l) {
		return "the Dockerfile doesn't parse"
	}
	if missingSourceRe.MatchString(l) {
		return "a COPY or ADD source isn't in the build context"
	}
	return ""
}

// flush ends any unterminated lines.
func (b *buildLog) flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.partial {
		if len(b.partial[i]) > 0 {
			b.line(string(b.partial[i]), b.long[i])
			b.partial[i], b.long[i] = nil, false
		}
	}
}

// failure is the error for a build that exited with pr, naming dockerfile:
// IMAGE_BUILD_FAILED, with the output's tail as its detail, when the
// output shows the Dockerfile at fault and nothing else failing; otherwise
// IMAGE_FAILED, spotexec's own failure.
func (b *buildLog) failure(dockerfile string, pr procResult, err error) error {
	b.flush()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.project == "" || b.infra || pr.startErr != nil || pr.timedOut || pr.canceled || pr.spotIntr || pr.signal != "" {
		return perr(protocol.CodeImageFailed, "docker build %s: %v", dockerfile, err)
	}
	return &protoError{
		code:   protocol.CodeImageBuildFailed,
		msg:    "the project's Dockerfile " + dockerfile + " failed to build: " + b.project,
		detail: strings.Join(b.tail, "\n"),
	}
}
