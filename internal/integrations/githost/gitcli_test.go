package githost

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hackclub/ari-webhooks/internal/testgit"
)

func gitAt(t *testing.T, dir, when string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=User One", "GIT_AUTHOR_EMAIL=User1@example.com",
		"GIT_COMMITTER_NAME=User One", "GIT_COMMITTER_EMAIL=user1@example.com",
		"GIT_AUTHOR_DATE="+when, "GIT_COMMITTER_DATE="+when,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, dir, name, contents string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func day(n int) string {
	return time.Date(2026, 6, n, 10, 0, 0, 0, time.UTC).Format(time.RFC3339)
}

func at(n int) time.Time {
	return time.Date(2026, 6, n, 10, 0, 0, 0, time.UTC)
}

// seven commits on a default branch named trunk, one a day from June 1st: a
// first commit, a rename, a side branch, co-author trailers, the merge of the
// side branch, a delete, and one more commit a week later
func buildHistory(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	gitAt(t, src, day(1), "init", "-q", "-b", "trunk")
	writeFile(t, src, "README.md", "# project1\n")
	writeFile(t, src, "src/a.txt", strings.Repeat("a line of project1\n", 20))
	writeFile(t, src, "docs/with space.md", "notes\n")
	gitAt(t, src, day(1), "add", ".")
	gitAt(t, src, day(1), "commit", "-q", "-m", "first")

	gitAt(t, src, day(2), "mv", "src/a.txt", "src/b.txt")
	writeFile(t, src, "src/b.txt", strings.Repeat("a line of project1\n", 20)+"one more\n")
	gitAt(t, src, day(2), "add", ".")
	gitAt(t, src, day(2), "commit", "-q", "-m", "rename a")

	gitAt(t, src, day(3), "checkout", "-q", "-b", "side")
	writeFile(t, src, "side.txt", "side\n")
	gitAt(t, src, day(3), "add", ".")
	gitAt(t, src, day(3), "commit", "-q", "-m", "side work")

	gitAt(t, src, day(4), "checkout", "-q", "trunk")
	writeFile(t, src, "main.txt", "main\nwork\n")
	gitAt(t, src, day(4), "add", ".")
	gitAt(t, src, day(4), "commit", "-q", "-m", strings.Join([]string{
		"a title on two lines",
		"that git folds into one",
		"",
		"Co-authored-by: Not A Trailer <user9@example.com>",
		"sits in the body, so it is prose.",
		"",
		"Co-authored-by: User Two <USER2@example.com>",
		"Co-authored-by: <user3@example.com>",
	}, "\n"))

	gitAt(t, src, day(5), "merge", "-q", "--no-ff", "side", "-m", "merge side")

	gitAt(t, src, day(6), "rm", "-q", "docs/with space.md")
	gitAt(t, src, day(6), "commit", "-q", "-m", "delete notes\n\nSigned-off-by: User One <user1@example.com>\nCo-authored-by: User Four <user4@example.com>")

	writeFile(t, src, "later.txt", "later\n")
	gitAt(t, src, day(13), "add", ".")
	gitAt(t, src, day(13), "commit", "-q", "-m", "after the ship")
	return src
}

// serveHistory publishes src as owner1/project1 on a git server that honors
// --filter, and returns the server, the bare repository and its url
func serveHistory(t *testing.T, src string) (srv *httptest.Server, bare, repoUrl string) {
	t.Helper()
	root := t.TempDir()
	bare = filepath.Join(root, "owner1", "project1")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	gitAt(t, src, day(1), "clone", "-q", "--bare", src, bare)
	gitAt(t, bare, day(1), "config", "uploadpack.allowFilter", "true")
	gitAt(t, bare, day(1), "config", "uploadpack.allowAnySHA1InWant", "true")
	srv = testgit.Serve(t, root)
	return srv, bare, srv.URL + "/owner1/project1"
}

func localFetcher() *Fetcher {
	f := NewFetcher(2, 0)
	f.AllowPrivateHosts = true // httptest listens on 127.0.0.1
	return f
}

func subjects(commits []Commit) []string {
	var out []string
	for _, commit := range commits {
		out = append(out, commit.Message)
	}
	return out
}

func sorted(values []string) []string {
	out := append([]string{}, values...)
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	return strings.Join(a, "\x00") == strings.Join(b, "\x00")
}

var shipWindow = Window{Since: at(2), Until: at(6)}

// The window and the cap are applied by git itself, both ends inclusive.
func TestGitWindowsCommitsAtTheSource(t *testing.T) {
	_, _, repoUrl := serveHistory(t, buildHistory(t))
	res := localFetcher().FetchCommits(context.Background(), repoUrl, shipWindow)
	if !res.OK || res.Error != "" || res.Source != "git" {
		t.Fatalf("fetch: %+v", res)
	}
	want := []string{"delete notes", "merge side", "a title on two lines that git folds into one", "side work", "rename a"}
	if !equal(subjects(res.Commits), want) {
		t.Fatalf("commits in the window, newest first:\n got %q\nwant %q", subjects(res.Commits), want)
	}
	if res.OutsideWindow != 2 {
		t.Fatalf("the first commit and the one after the ship sit outside: %d", res.OutsideWindow)
	}
	for _, commit := range res.Commits {
		if commit.LineStats || commit.Additions != 0 || commit.Deletions != 0 {
			t.Fatalf("a clone counts no lines: %+v", commit)
		}
	}
	byMessage := map[string]Commit{}
	for _, commit := range res.Commits {
		byMessage[commit.Message] = commit
	}
	if got := sorted(byMessage["rename a"].Paths); !equal(got, []string{"src/a.txt", "src/b.txt"}) {
		t.Fatalf("a rename touches both paths: %q", got)
	}
	if got := byMessage["delete notes"].Paths; !equal(got, []string{"docs/with space.md"}) {
		t.Fatalf("a delete is a touched path, spaces kept: %q", got)
	}
	if got := byMessage["merge side"].Paths; len(got) != 0 {
		t.Fatalf("a merge has no diff of its own: %q", got)
	}
	titled := byMessage["a title on two lines that git folds into one"]
	if len(titled.CoAuthors) != 2 || *titled.CoAuthors[0].Email != "user2@example.com" || *titled.CoAuthors[1].Email != "user3@example.com" {
		t.Fatalf("only the trailer block names co-authors: %+v", titled.CoAuthors)
	}
	if titled.AuthorName != "User One" || titled.AuthorEmail != "user1@example.com" || !titled.CommittedAt.Equal(at(4)) {
		t.Fatalf("author and time: %+v", titled)
	}

	wantHistory := []string{"docs/with space.md", "later.txt", "main.txt", "side.txt", "src/a.txt", "src/b.txt"}
	if got := sorted(res.HistoryPaths); !equal(got, wantHistory) {
		t.Fatalf("history paths run from the window's start to the branch head:\n got %q\nwant %q", got, wantHistory)
	}
	if !res.TreeRead || res.Readme != "README.md" {
		t.Fatalf("tree: read=%v readme=%q", res.TreeRead, res.Readme)
	}
}

func TestGitWindowBoundsAreExact(t *testing.T) {
	_, _, repoUrl := serveHistory(t, buildHistory(t))
	f := localFetcher()
	justInside := f.FetchCommits(context.Background(), repoUrl, Window{Since: at(2).Add(-999 * time.Millisecond), Until: at(6).Add(999 * time.Millisecond)})
	if len(justInside.Commits) != 5 {
		t.Fatalf("a bound within the same second keeps the edge commits: %q", subjects(justInside.Commits))
	}
	justOutside := f.FetchCommits(context.Background(), repoUrl, Window{Since: at(2).Add(time.Millisecond), Until: at(6).Add(-time.Millisecond)})
	if !equal(subjects(justOutside.Commits), []string{"merge side", "a title on two lines that git folds into one", "side work"}) {
		t.Fatalf("a millisecond inside each edge drops both edge commits: %q", subjects(justOutside.Commits))
	}
	open := f.FetchCommits(context.Background(), repoUrl, Window{})
	if len(open.Commits) != 7 || open.OutsideWindow != 0 {
		t.Fatalf("an open window is the whole branch: %d commits, %d outside", len(open.Commits), open.OutsideWindow)
	}
}

func TestGitTruncatesAtTheCapInsideTheWindow(t *testing.T) {
	_, _, repoUrl := serveHistory(t, buildHistory(t))
	f := localFetcher()
	f.maxCommits = 3
	res := f.FetchCommits(context.Background(), repoUrl, shipWindow)
	if !res.OK || res.Error != "truncated" {
		t.Fatalf("a capped history still succeeds and says so: %+v", res)
	}
	if !equal(subjects(res.Commits), []string{"delete notes", "merge side", "a title on two lines that git folds into one"}) {
		t.Fatalf("the cap keeps the newest commits of the window: %q", subjects(res.Commits))
	}
}

// The clone brings commits and trees only. With the remote gone and lazy
// fetching refused, the log and the listing must still answer: nothing they read
// is a file's contents.
func TestGitLogAndTreeNeedNoFileContents(t *testing.T) {
	srv, _, repoUrl := serveHistory(t, buildHistory(t))
	f := localFetcher()
	clone := gitBackend{fetcher: f}.open(context.Background(), f.repoRef(repoUrl), time.Minute, false)
	defer clone.release()
	if clone.code != "" {
		t.Fatalf("clone: %s", clone.code)
	}
	srv.Close()

	missing := runGit(context.Background(), []string{"rev-list", "--objects", "--missing=print", "HEAD"}, clone.dir, time.Minute, offline)
	if !missing.ok || strings.Count(missing.stdout, "\n?") == 0 {
		t.Fatalf("the clone must hold no file contents: %s %s", missing.stdout, missing.stderr)
	}

	logResult := runGit(context.Background(), logArgs(shipWindow, 1000), clone.dir, time.Minute, offline)
	if !logResult.ok {
		t.Fatalf("log without the remote: %s", logResult.stderr)
	}
	commits := ParseLog(logResult.stdout, repoUrl)
	if len(commits) != 5 || len(historyPaths(commits)) != 5 {
		t.Fatalf("commits and their paths come from trees alone: %q %q", subjects(commits), historyPaths(commits))
	}
	treeResult := runGit(context.Background(), []string{"ls-tree", "-r", "--long", "-z", "HEAD"}, clone.dir, time.Minute, offline)
	if !treeResult.ok {
		t.Fatalf("ls-tree without the remote: %s", treeResult.stderr)
	}
	files := ParseTree(treeResult.stdout)
	if len(files) != 5 {
		t.Fatalf("files: %+v", files)
	}
	for _, file := range files {
		if len(file.Blob) != 40 || file.Bytes != UnknownBytes {
			t.Fatalf("blob ids without sizes: %+v", file)
		}
	}

	stats := runGit(context.Background(), []string{"log", "--numstat", "--format=%H"}, clone.dir, time.Minute, offline)
	if stats.ok {
		t.Fatal("counting lines needs file contents: this is the read the capture no longer makes")
	}
}

// One blob is all a preview needs: the clone fetches it on demand.
func TestGitReadsOneFileFromABloblessClone(t *testing.T) {
	_, _, repoUrl := serveHistory(t, buildHistory(t))
	f := localFetcher()
	got := f.FetchFile(context.Background(), repoUrl, "main.txt")
	if !got.OK || got.Content != "main\nwork\n" {
		t.Fatalf("file: %+v", got)
	}
	repo := f.FetchRepoContext(context.Background(), repoUrl)
	if !repo.OK || repo.Error != "" || repo.ReadmeName != "README.md" || repo.Readme != "# project1\n" {
		t.Fatalf("readme: %+v", repo)
	}
	if !equal(sorted(repo.Paths), []string{"README.md", "later.txt", "main.txt", "side.txt", "src/b.txt"}) {
		t.Fatalf("paths: %q", repo.Paths)
	}
}

// rejectFilters fronts a git server and refuses any fetch that asks for a filter,
// the way a host without partial clone support does.
func rejectFilters(t *testing.T, upstream string) (srv *httptest.Server, filtered, plain *atomic.Int32) {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	filtered, plain = &atomic.Int32{}, &atomic.Int32{}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			request := body
			if r.Header.Get("content-encoding") == "gzip" {
				if unzipped, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
					request, _ = io.ReadAll(unzipped)
				}
			}
			if bytes.Contains(request, []byte("command=fetch")) {
				if bytes.Contains(request, []byte("filter blob:none")) {
					filtered.Add(1)
					w.Header().Set("content-type", "application/x-git-upload-pack-result")
					refusal := "ERR filter 'blob:none' is not supported"
					fmt.Fprintf(w, "%04x%s", len(refusal)+4, refusal) // one pkt-line: git reports it as a remote error
					return
				}
				plain.Add(1)
			}
		}
		forward, _ := http.NewRequest(r.Method, target.String()+r.URL.RequestURI(), bytes.NewReader(body))
		forward.Header = r.Header.Clone()
		res, err := http.DefaultTransport.RoundTrip(forward)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer res.Body.Close()
		for name, values := range res.Header {
			w.Header()[name] = values
		}
		w.WriteHeader(res.StatusCode)
		io.Copy(w, res.Body)
	}))
	t.Cleanup(srv.Close)
	return srv, filtered, plain
}

func TestGitClonesUnfilteredWhenTheServerRejectsFilters(t *testing.T) {
	upstream, _, _ := serveHistory(t, buildHistory(t))
	srv, filtered, plain := rejectFilters(t, upstream.URL)

	res := localFetcher().FetchCommits(context.Background(), srv.URL+"/owner1/project1", shipWindow)
	if !res.OK || len(res.Commits) != 5 {
		t.Fatalf("a rejected filter must fall back to a plain clone: %+v", res)
	}
	if filtered.Load() == 0 || plain.Load() == 0 {
		t.Fatalf("expected a refused filtered fetch then a plain one: filtered=%d plain=%d", filtered.Load(), plain.Load())
	}
	for _, file := range res.Files {
		if file.Bytes == UnknownBytes {
			t.Fatalf("a plain clone holds the contents, so sizes are known: %+v", file)
		}
	}
}

func TestGitCloneTimeoutIsTransientAndNamed(t *testing.T) {
	_, _, repoUrl := serveHistory(t, buildHistory(t))
	f := NewFetcher(2, time.Millisecond)
	f.AllowPrivateHosts = true
	res := f.FetchCommits(context.Background(), repoUrl, Window{})
	if res.OK || !res.Transient || !res.TimedOut || errorCode(res.Error) != "clone_timeout" {
		t.Fatalf("a clone cut by the timeout is clone_timeout: %+v", res)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	full := localFetcher()
	full.slots <- struct{}{}
	full.slots <- struct{}{}
	waiting := full.FetchCommits(canceled, repoUrl, Window{})
	if waiting.OK || !waiting.Transient || errorCode(waiting.Error) != "clone_timeout" {
		t.Fatalf("giving up while every slot is busy is a timeout, not a missing repo: %+v", waiting)
	}
}

func TestGitEmptyRepositoryIsNotTransient(t *testing.T) {
	root := t.TempDir()
	gitAt(t, root, day(1), "init", "-q", "--bare", "-b", "main", filepath.Join(root, "empty.git"))
	srv := testgit.Serve(t, root)
	res := localFetcher().FetchCommits(context.Background(), srv.URL+"/empty.git", Window{})
	if res.OK || res.Transient || errorCode(res.Error) != "log_failed" {
		t.Fatalf("a repository without commits is log_failed and final: %+v", res)
	}
}
