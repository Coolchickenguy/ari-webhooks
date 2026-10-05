package githost

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// gitBackend reads any host with the git CLI: a temporary bare clone without
// file contents, then log and ls-tree on it.
type gitBackend struct {
	fetcher *Fetcher
}

// log and ls-tree must answer from the commits and trees the clone brought:
// without this a read that wants a missing blob goes back out to the remote, one
// object at a time
const offline = "GIT_NO_LAZY_FETCH=1"

// logArgs lists commits newest first with the paths each one touched. Paths come
// from comparing trees, so no file contents are read; --no-renames keeps it that
// way (rename detection compares contents) and reports a rename as both paths.
func logArgs(window Window, limit int) []string {
	args := []string{
		"log", "--max-count=" + strconv.Itoa(limit),
		"--format=%x01%H%x1f%cI%x1f%an%x1f%ae%x1f%s%x1f%(trailers:key=Co-authored-by,valueonly)%x02",
		"--name-only", "--no-renames", "-z",
	}
	if !window.Since.IsZero() {
		since := window.Since.UTC().Truncate(time.Second)
		if since.Before(window.Since) {
			since = since.Add(time.Second)
		}
		// as a filter: plain --since stops walking at the first older commit, and
		// history with out-of-order dates would lose commits behind it
		args = append(args, "--since-as-filter="+since.Format(time.RFC3339))
	}
	if !window.Until.IsZero() {
		args = append(args, "--until="+window.Until.UTC().Truncate(time.Second).Format(time.RFC3339))
	}
	return args
}

type openClone struct {
	dir string
	// code is the failed step's error, "" when the clone is ready
	code     string
	timedOut bool
	release  func()
}

// open clones repo into a temporary directory while holding a slot. release frees
// both and must be called whatever the outcome.
func (g gitBackend) open(ctx context.Context, repo repoRef, timeout time.Duration, headOnly bool) openClone {
	f := g.fetcher
	cloneUrl := f.resolveRedirectedBase(ctx, repo.cloneUrl)

	tmpBase, err := os.MkdirTemp("", "ari-git-")
	if err != nil {
		return openClone{code: "clone_failed: " + err.Error(), release: func() {}}
	}
	select {
	case f.slots <- struct{}{}:
	case <-ctx.Done():
		os.RemoveAll(tmpBase)
		return openClone{code: "clone_timeout: canceled before a clone slot freed", timedOut: true, release: func() {}}
	}
	out := openClone{dir: filepath.Join(tmpBase, "repo"), release: func() {
		<-f.slots
		os.RemoveAll(tmpBase)
	}}

	cloneArgs := func(filtered bool) []string {
		args := []string{
			"-c", "http.followRedirects=false", // a server must not bounce git into the internal network after the DNS check
			"-c", "pack.threads=1", // index-pack defaults to one thread per core; concurrent clones at that width OOM a small box
			"-c", "core.bigFileThreshold=1m", // index-pack streams blobs past this to disk instead of inflating them in memory
			"clone", "--bare",
		}
		if filtered {
			args = append(args, "--filter=blob:none")
		}
		args = append(args, "--single-branch", "--no-tags")
		if headOnly {
			args = append(args, "--depth", "1")
		}
		return append(args, "--", cloneUrl, out.dir)
	}
	cloneResult := runGit(ctx, cloneArgs(true), "", timeout)
	if !cloneResult.ok && !cloneResult.timedOut && strings.Contains(strings.ToLower(cloneResult.stderr), "filter") {
		os.RemoveAll(out.dir)
		cloneResult = runGit(ctx, cloneArgs(false), "", timeout)
	}
	if cloneResult.ok {
		return out
	}

	reason := gitFailReason(cloneResult, timeout)
	out.code = "clone_failed: " + reason
	out.timedOut = cloneResult.timedOut
	if cloneResult.timedOut {
		out.code = "clone_timeout: " + reason // a slow host or saturated pool, almost never proof the repo is gone
	} else if tlsInfraError.MatchString(reason) {
		out.code = "clone_infra: " + reason // OUR environment (CA roots), never evidence about the repo
	}
	return out
}

func (g gitBackend) commits(ctx context.Context, repo repoRef, window Window) (Result, *apiFailure) {
	f := g.fetcher
	clone := g.open(ctx, repo, f.cloneTimeout, false)
	defer clone.release()
	if clone.code != "" {
		return Result{Transient: true, TimedOut: clone.timedOut, Error: clone.code, Source: "git"}, nil
	}

	readLog := func(window Window) ([]Commit, bool, *Result) {
		logResult := runGit(ctx, logArgs(window, f.maxCommits), clone.dir, f.cloneTimeout, offline)
		if logResult.timedOut {
			return nil, false, &Result{Transient: true, TimedOut: true, Error: "log_timeout: " + gitFailReason(logResult, f.cloneTimeout), Source: "git"}
		}
		if !logResult.ok {
			return nil, false, &Result{Error: "log_failed: " + gitFailReason(logResult, f.cloneTimeout), Source: "git"}
		}
		stdout := logResult.stdout
		if logResult.truncated {
			if i := strings.LastIndex(stdout, "\x01"); i >= 0 {
				stdout = stdout[:i] // the final block is cut mid-commit; the parser must never see half a record
			}
		}
		var commits []Commit
		for _, commit := range ParseLog(stdout, repo.webUrl) {
			if window.holds(commit.CommittedAt) {
				commits = append(commits, commit)
			}
		}
		return commits, logResult.truncated, nil
	}

	commits, logTruncated, failed := readLog(window)
	if failed != nil {
		return *failed, nil
	}
	var later []Commit
	if !window.Until.IsZero() {
		// commits pushed after the ship still say which paths this repository held
		later, _, failed = readLog(Window{Since: window.Until.Add(time.Millisecond)})
		if failed != nil {
			return *failed, nil
		}
	}
	out := Result{OK: true, Commits: commits, HistoryPaths: historyPaths(commits, later), Source: "git"}

	countResult := runGit(ctx, []string{"rev-list", "--count", "HEAD"}, clone.dir, f.cloneTimeout, offline)
	if total, err := strconv.Atoi(strings.TrimSpace(countResult.stdout)); countResult.ok && err == nil && total > len(commits) {
		out.OutsideWindow = total - len(commits)
	}

	// Recursive listing of the default branch. It reads trees only: the blob id is
	// recorded in the tree, and a size is reported only for blobs the clone holds.
	// Never fatal: a repo whose listing cannot be read is reported as unknown so the
	// checks that depend on it stay silent rather than guessing.
	treeResult := runGit(ctx, []string{"ls-tree", "-r", "--long", "-z", "HEAD"}, clone.dir, f.cloneTimeout, offline)
	if treeResult.ok && !treeResult.truncated {
		out.TreeRead = true
		out.Files = ParseTree(treeResult.stdout)
		names := make([]string, 0, len(out.Files))
		for _, entry := range out.Files {
			names = append(names, entry.Path)
		}
		out.Readme = FindReadme(names)
	}

	if len(commits) >= f.maxCommits || logTruncated {
		out.Error = "truncated"
	}
	return out, nil
}

// readBlob faults in exactly one blob from the promisor remote; redirects stay
// disabled for that fetch too.
func readBlob(ctx context.Context, repoDir, filePath string, timeout time.Duration) gitRun {
	return runGit(ctx, []string{
		"-c", "http.followRedirects=false", // the promisor blob fetch must not bounce past the DNS check either
		"-c", "core.bigFileThreshold=1m",
		"cat-file", "blob", "HEAD:" + filePath,
	}, repoDir, timeout)
}

func (g gitBackend) file(ctx context.Context, repo repoRef, filePath string) (FileContent, *apiFailure) {
	timeout := g.fetcher.readTimeout()
	clone := g.open(ctx, repo, timeout, true)
	defer clone.release()
	if clone.code != "" {
		return FileContent{Error: clone.code}, nil
	}
	readResult := readBlob(ctx, clone.dir, filePath, timeout)
	if !readResult.ok {
		return FileContent{Error: "read_failed: " + gitFailReason(readResult, timeout)}, nil
	}
	return previewOf(readResult.stdout, readResult.truncated), nil
}

func (g gitBackend) repoContext(ctx context.Context, repo repoRef) (RepoContext, *apiFailure) {
	timeout := g.fetcher.readTimeout()
	clone := g.open(ctx, repo, timeout, true)
	defer clone.release()
	if clone.code != "" {
		return RepoContext{Error: clone.code}, nil
	}

	treeResult := runGit(ctx, []string{"ls-tree", "-r", "--name-only", "-z", "HEAD"}, clone.dir, timeout, offline)
	if !treeResult.ok {
		return RepoContext{Error: "tree_failed: " + gitFailReason(treeResult, timeout)}, nil
	}
	out := RepoContext{OK: true, PathsTruncated: treeResult.truncated}
	for _, name := range strings.Split(treeResult.stdout, "\x00") {
		if name != "" {
			out.Paths = append(out.Paths, name)
		}
	}
	if treeResult.truncated && len(out.Paths) > 0 {
		out.Paths = out.Paths[:len(out.Paths)-1] // the final record can be cut mid-path
	}

	out.ReadmeName = FindReadme(out.Paths)
	if out.ReadmeName == "" {
		return out, nil
	}
	readResult := readBlob(ctx, clone.dir, out.ReadmeName, timeout)
	if !readResult.ok {
		out.Error = "readme_failed: " + gitFailReason(readResult, timeout)
		return out, nil
	}
	out.Readme = readResult.stdout
	out.ReadmeTruncated = readResult.truncated
	if len(out.Readme) > MaxPreviewBytes {
		out.Readme = out.Readme[:MaxPreviewBytes]
		out.ReadmeTruncated = true
	}
	return out, nil
}
