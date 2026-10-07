package githost

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

type Commit struct {
	Hash        string
	ShortHash   string
	Message     string
	CommittedAt time.Time
	Additions   int
	Deletions   int
	// LineStats is false when the source could not count lines: a clone without
	// file contents has nothing to diff. Additions and Deletions are then zero and
	// mean unknown, not "no lines changed".
	LineStats   bool
	AuthorName  string
	AuthorEmail string
	CoAuthors   []CoAuthor
	// Paths is every path the commit touched, both sides of a rename. Empty for a merge.
	Paths []string
	Url   string
}

type CoAuthor struct {
	Name  *string `json:"name"`
	Email *string `json:"email"`
}

// Window bounds the commits a capture wants by committed time, both ends
// inclusive. A zero end is open.
type Window struct {
	Since time.Time
	Until time.Time
}

func (w Window) holds(t time.Time) bool {
	return (w.Since.IsZero() || !t.Before(w.Since)) && (w.Until.IsZero() || !t.After(w.Until))
}

type Result struct {
	OK bool
	// Transient marks retryable failures (network/timeout); false for bad URL or empty repo.
	Transient bool
	// TimedOut marks a clone killed by our timeout (slow host or saturated pool).
	TimedOut bool
	// Commits is the default branch's commits inside the window, newest first.
	Commits []Commit
	Error   string
	// OutsideWindow counts the default branch's commits the window left out.
	OutsideWindow int
	// Source names the backend that answered: "github" or "git".
	Source string
	// Notes carry what happened on the way, such as an api read that fell back to a clone.
	Notes []string
	// TreeRead reports that the default branch's root listing was actually read.
	// False leaves Readme meaningless: absence of evidence, not evidence of absence.
	TreeRead bool
	// Readme is the root README's filename on the default branch, "" when there is none.
	Readme string
	// Files is every blob on the default branch with its git blob id and size.
	// Only populated when TreeRead is true.
	Files []File
	// HistoryPaths is every path touched by the commits from the window's start to
	// the branch head, including the ones deleted or renamed away. A file missing
	// from Files but present here did exist in this repository.
	HistoryPaths []string
}

// File is one path on the default branch. Blob is the git blob id, a hash of the
// file's exact bytes: two files with the same Blob are byte-identical, whatever
// they are named and whichever repo they came from. Bytes is UnknownBytes when the
// source lists files without their contents.
type File struct {
	Path  string
	Blob  string
	Bytes int64
}

const UnknownBytes int64 = -1

// FileContent is one file read from a repository's default branch, fetched on
// demand for the review screen's source viewer.
type FileContent struct {
	OK bool
	// Content is the file's bytes as a string, cut at MaxPreviewBytes when Truncated.
	Content   string
	Truncated bool
	// Binary marks a file whose leading bytes contain a NUL (git's own text
	// heuristic); Content is empty.
	Binary bool
	Error  string
}

// MaxPreviewBytes caps what the source viewer carries per file. Also enforced by
// the internal API against the capture-time size before any read happens.
const MaxPreviewBytes = 512 << 10

// RepoContext is the default branch's file listing plus the root README's
// content, read live for the AI checks and the review screen's README tab.
type RepoContext struct {
	OK bool
	// Paths is every path on the default branch. Only meaningful when OK.
	Paths []string
	// PathsTruncated marks a listing cut short.
	PathsTruncated bool
	// ReadmeName is the root README's filename, "" when the repository has none.
	ReadmeName string
	// Readme is the README's content, cut at MaxPreviewBytes when ReadmeTruncated.
	Readme          string
	ReadmeTruncated bool
	// Error carries the failed step's code. It can be set with OK true when the
	// listing was read but the README itself could not be.
	Error string
}

type repoRef struct {
	cloneUrl string
	// webUrl is the base commit links are built on
	webUrl string
	// owner and name are set for a repository on the GitHub host
	owner string
	name  string
}

// apiFailure is an api backend saying it cannot answer, so the next backend is asked
type apiFailure struct {
	reason string
	// transient: the api itself was unreachable or throttled, which says nothing about the repository
	transient bool
	// quiet: already logged where it happened, or not worth a line per capture
	quiet bool
}

type backend interface {
	commits(ctx context.Context, repo repoRef, window Window) (Result, *apiFailure)
	file(ctx context.Context, repo repoRef, filePath string) (FileContent, *apiFailure)
	repoContext(ctx context.Context, repo repoRef) (RepoContext, *apiFailure)
}

type Fetcher struct {
	slots        chan struct{}
	cloneTimeout time.Duration
	maxCommits   int
	// AllowPrivateHosts bypasses the SSRF guard, including the dial-time check, for httptest git servers. TEST ONLY.
	AllowPrivateHosts bool
	// GitHub reads repositories on its host through the api proxy. Nil, or without
	// the proxy's url and key, every host is cloned.
	GitHub *GitHub
}

// NewFetcher bounds concurrent clones so a burst of submissions cannot starve
// CPU/disk/file descriptors into false inaccessible_repo rejections, or OOM a
// memory-limited host. cloneTimeout bounds each step of one capture attempt; the
// timeout-retry loop reuses the same budget every attempt, so a value that is too
// small makes large repos permanently uncapturable.
func NewFetcher(concurrency int, cloneTimeout time.Duration) *Fetcher {
	if concurrency < 1 {
		concurrency = 4
	}
	if cloneTimeout <= 0 {
		cloneTimeout = 5 * time.Minute
	}
	return &Fetcher{
		slots:        make(chan struct{}, concurrency),
		cloneTimeout: cloneTimeout,
		maxCommits:   1000, // enrich reports the cap as commit_history_truncated_at_1000
	}
}

// a reviewer or a queued check waits on file and README reads, so they get a
// short budget instead of the capture's multi-minute one
func (f *Fetcher) readTimeout() time.Duration {
	if f.cloneTimeout > time.Minute {
		return time.Minute
	}
	return f.cloneTimeout
}

func (f *Fetcher) repoRef(repoUrl string) repoRef {
	if f.GitHub != nil {
		if repo, ok := f.GitHub.repoRef(repoUrl); ok {
			return repo
		}
	}
	return repoRef{cloneUrl: repoUrl, webUrl: NormalizeRepoUrl(repoUrl)}
}

func logSkip(repo repoRef, failure *apiFailure) {
	if !failure.quiet {
		slog.Warn("github api skipped", "repo", repo.webUrl, "reason", failure.reason)
	}
}

func (f *Fetcher) backends(repo repoRef) []backend {
	git := gitBackend{fetcher: f}
	if repo.owner != "" && f.GitHub.configured() {
		return []backend{githubBackend{api: f.GitHub, fetcher: f}, git}
	}
	return []backend{git}
}

// FetchCommits reads the default branch: its commits inside the window, the paths
// they touched and the file listing. A GitHub repository is read through the api
// proxy when one is configured; anything the api cannot answer, and every other
// host, goes through a blobless clone.
func (f *Fetcher) FetchCommits(ctx context.Context, repoUrl string, window Window) Result {
	if code, transient, ok := f.guardRepoUrl(ctx, repoUrl); !ok {
		return Result{Transient: transient, Error: code}
	}
	repo := f.repoRef(repoUrl)
	var skipped *apiFailure
	var out Result
	for _, source := range f.backends(repo) {
		res, failure := source.commits(ctx, repo, window)
		if failure != nil {
			logSkip(repo, failure)
			skipped = failure
			continue
		}
		out = res
		break
	}
	if skipped == nil {
		return out
	}
	out.Notes = append(out.Notes, "github_api_skipped: "+skipped.reason)
	if skipped.transient && !out.OK && errorCode(out.Error) == "clone_failed" {
		// neither path reached the repository, and the api failure was GitHub's own:
		// that is not evidence the repository is gone, so it must not start the
		// inaccessible-repository countdown
		out.Error = "github_unavailable: " + skipped.reason + "; " + strings.TrimPrefix(out.Error, "clone_failed: ")
		out.Transient = true
	}
	return out
}

// FetchFile reads one file from the default branch of repoUrl, live. Nothing
// survives the call.
func (f *Fetcher) FetchFile(ctx context.Context, repoUrl, filePath string) FileContent {
	// The path reaches git as one "HEAD:<path>" argument, so option injection is
	// impossible; still refuse shapes no tree path can have.
	if filePath == "" || strings.HasPrefix(filePath, "/") || strings.HasPrefix(filePath, "-") || strings.IndexByte(filePath, 0) >= 0 {
		return FileContent{Error: "bad_path"}
	}
	if code, _, ok := f.guardRepoUrl(ctx, repoUrl); !ok {
		return FileContent{Error: code}
	}
	repo := f.repoRef(repoUrl)
	var out FileContent
	for _, source := range f.backends(repo) {
		res, failure := source.file(ctx, repo, filePath)
		if failure != nil {
			logSkip(repo, failure)
			continue
		}
		out = res
		break
	}
	return out
}

// FetchRepoContext lists the default branch and reads its root README, live.
func (f *Fetcher) FetchRepoContext(ctx context.Context, repoUrl string) RepoContext {
	if code, _, ok := f.guardRepoUrl(ctx, repoUrl); !ok {
		return RepoContext{Error: code}
	}
	repo := f.repoRef(repoUrl)
	var out RepoContext
	for _, source := range f.backends(repo) {
		res, failure := source.repoContext(ctx, repo)
		if failure != nil {
			logSkip(repo, failure)
			continue
		}
		out = res
		break
	}
	return out
}
