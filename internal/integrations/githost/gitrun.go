package githost

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	fatalLine     = regexp.MustCompile(`(?i)^(fatal|error):`)
	tlsInfraError = regexp.MustCompile(`(?i)certificate verification failed|unable to get local issuer|SSL certificate problem`)
)

type gitRun struct {
	ok        bool
	stdout    string
	stderr    string
	timedOut  bool
	truncated bool
}

// cappedWriter keeps at most max bytes and drops the rest, so one pathological
// repo's log output cannot buffer away the whole box's memory.
type cappedWriter struct {
	b         strings.Builder
	max       int
	truncated bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	n := len(p)
	room := w.max - w.b.Len()
	if room <= 0 {
		if n > 0 {
			w.truncated = true
		}
		return n, nil
	}
	if n > room {
		w.truncated = true
		p = p[:room]
	}
	w.b.Write(p)
	return n, nil
}

func runGit(ctx context.Context, args []string, dir string, timeout time.Duration, env ...string) gitRun {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", // never prompt for credentials; fail fast on private repos
		"GIT_ASKPASS=echo",
	), env...)
	cmd.WaitDelay = 2 * time.Second        // force-kill grace: git blocked on network IO can ignore SIGTERM
	stdout := &cappedWriter{max: 64 << 20} // 64MB of log or listing covers any legitimate repo
	stderr := &cappedWriter{max: 64 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	timedOut := ctx.Err() == context.DeadlineExceeded
	if err != nil && stderr.b.Len() == 0 {
		stderr.b.WriteString("error: ")
		stderr.b.WriteString(err.Error())
	}
	return gitRun{ok: err == nil && !timedOut, stdout: stdout.b.String(), stderr: stderr.b.String(), timedOut: timedOut, truncated: stdout.truncated}
}

// gitFailReason picks the most informative stderr line (git prints fatal last).
func gitFailReason(r gitRun, timeout time.Duration) string {
	if r.timedOut {
		return "timed out after " + strconv.Itoa(int(timeout.Seconds())) + "s"
	}
	var lines []string
	for _, l := range strings.Split(r.stderr, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			lines = append(lines, t)
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if fatalLine.MatchString(lines[i]) {
			return sliceRunes(lines[i], 200)
		}
	}
	if len(lines) > 0 {
		return sliceRunes(lines[len(lines)-1], 200)
	}
	return "git exited nonzero"
}
