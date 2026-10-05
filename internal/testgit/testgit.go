package testgit

import (
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run executes git in dir with a fixed author identity and deterministic dates.
func Run(t *testing.T, dir string, args ...string) {
	t.Helper()
	RunAt(t, dir, "2026-06-20T10:00:00Z", args...)
}

// RunAt is Run at a chosen commit time.
func RunAt(t *testing.T, dir, when string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Mia Maker", "GIT_AUTHOR_EMAIL=mia@example.com",
		"GIT_COMMITTER_NAME=Mia Maker", "GIT_COMMITTER_EMAIL=mia@example.com",
		"GIT_AUTHOR_DATE="+when, "GIT_COMMITTER_DATE="+when,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// httpBackend finds a git whose http-backend actually exists. GitHub Desktop's
// relocatable git reports a broken --exec-path, so candidates are probed.
func httpBackend(t *testing.T) string {
	t.Helper()
	for _, gitBin := range []string{"git", "/usr/bin/git", "/opt/homebrew/bin/git", "/usr/local/bin/git"} {
		out, err := exec.Command(gitBin, "--exec-path").Output()
		if err != nil {
			continue
		}
		backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
		if _, err := os.Stat(backend); err == nil {
			return backend
		}
	}
	t.Skip("no git-http-backend available")
	return ""
}

// Serve exposes a directory of bare repos over the smart HTTP protocol
// (shallow clones need it; dumb HTTP refuses --depth).
func Serve(t *testing.T, root string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(&cgi.Handler{
		Path: httpBackend(t),
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	})
	t.Cleanup(srv.Close)
	return srv
}
