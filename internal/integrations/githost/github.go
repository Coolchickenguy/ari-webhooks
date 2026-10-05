package githost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hackclub/ari-webhooks/internal/httpx"
)

var repoSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// repoRef recognises a repository on this host whatever was pasted after it:
// a .git suffix, a trailing slash, or a /tree/<branch> page.
func (g *GitHub) repoRef(repoUrl string) (repoRef, bool) {
	u, err := url.Parse(repoUrl)
	if err != nil || u.User != nil {
		return repoRef{}, false
	}
	if strings.TrimPrefix(strings.ToLower(u.Host), "www.") != g.host {
		return repoRef{}, false
	}
	var segments []string
	for _, segment := range strings.Split(u.Path, "/") {
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	if len(segments) < 2 {
		return repoRef{}, false
	}
	owner, name := segments[0], trailingGit.ReplaceAllString(segments[1], "")
	if !repoSegment.MatchString(owner) || !repoSegment.MatchString(name) || strings.Trim(name, ".") == "" {
		return repoRef{}, false
	}
	web := u.Scheme + "://" + g.host + "/" + owner + "/" + name
	return repoRef{cloneUrl: web, webUrl: web, owner: owner, name: name}, true
}

type githubRepo struct {
	FullName string `json:"full_name"`
}

func (r githubRepo) path() string {
	return "/repos/" + r.FullName
}

type githubCommit struct {
	Sha    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"author"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
	Parents []struct {
		Sha string `json:"sha"`
	} `json:"parents"`
	Stats struct {
		Additions int `json:"additions"`
		Deletions int `json:"deletions"`
	} `json:"stats"`
	Files []struct {
		Filename         string `json:"filename"`
		PreviousFilename string `json:"previous_filename"`
	} `json:"files"`
}

// repo follows the redirect a renamed repository answers with and returns its
// current owner and name, which every later call then uses directly.
func (g *GitHub) repo(ctx context.Context, ref repoRef) (githubRepo, bool, *apiFailure) {
	var info githubRepo
	reply, failure := g.getJson(ctx, "/repos/"+ref.owner+"/"+ref.name, nil, &info)
	if failure != nil {
		return info, false, failure
	}
	if reply.status != http.StatusOK {
		return info, false, nil
	}
	owner, name, isPair := strings.Cut(info.FullName, "/")
	if !isPair || !repoSegment.MatchString(owner) || !repoSegment.MatchString(name) {
		return info, false, &apiFailure{reason: "bad_response"}
	}
	return info, true, nil
}

var lastPage = regexp.MustCompile(`[?&]page=(\d+)[^>]*>;\s*rel="last"`)

type githubHead struct {
	// sha is "" when the default branch has no commits
	sha string
	// total is how many commits the default branch holds
	total int
	// found is false when GitHub has no such repository to show
	found bool
}

// head asks for the newest commit of the default branch. The proxy caches by
// url, and a cached answer could be minutes behind a push: the until bound moves
// every second, so this read is always a fresh one. Everything read after it is
// addressed by this commit, which a cache cannot make stale.
func (g *GitHub) head(ctx context.Context, repoPath string) (githubHead, *apiFailure) {
	var batch []githubCommit
	reply, failure := g.getJson(ctx, repoPath+"/commits", url.Values{
		"per_page": {"1"},
		"until":    {g.now().UTC().Add(24 * time.Hour).Format(time.RFC3339)}, // a day ahead, so a commit dated by a fast clock still counts
	}, &batch)
	if failure != nil {
		return githubHead{}, failure
	}
	if reply.status == http.StatusNotFound || reply.status == http.StatusUnavailableForLegalReasons {
		return githubHead{}, nil
	}
	if reply.status != http.StatusOK || len(batch) == 0 || len(batch[0].Sha) != 40 || !isHex(batch[0].Sha) {
		return githubHead{found: true}, nil
	}
	out := githubHead{found: true, sha: batch[0].Sha, total: 1}
	// only the page number is read: the link itself names GitHub's host and is never followed
	if m := lastPage.FindStringSubmatch(reply.header.Get("link")); m != nil {
		out.total, _ = strconv.Atoi(m[1])
	}
	return out, nil
}

// list pages back from the head commit newest first, keeping commits inside the
// window until limit. The api is asked for a second more on each side and the
// exact bounds are applied here, so its own rounding cannot drop an edge commit.
// Naming the head commit rather than the branch means a cached page is still a
// page of exactly this history.
func (g *GitHub) list(ctx context.Context, repo githubRepo, head string, window Window, limit int) ([]githubCommit, *apiFailure) {
	query := url.Values{"sha": {head}, "per_page": {strconv.Itoa(g.pageSize)}}
	if !window.Since.IsZero() {
		query.Set("since", window.Since.UTC().Add(-time.Second).Format(time.RFC3339))
	}
	if !window.Until.IsZero() {
		query.Set("until", window.Until.UTC().Add(time.Second).Format(time.RFC3339))
	}
	var kept []githubCommit
	for page := 1; page <= limit/g.pageSize+2; page++ {
		query.Set("page", strconv.Itoa(page))
		var batch []githubCommit
		reply, failure := g.getJson(ctx, repo.path()+"/commits", query, &batch)
		if failure != nil {
			return nil, failure
		}
		if reply.status != http.StatusOK {
			break
		}
		for _, commit := range batch {
			if len(commit.Sha) == 40 && isHex(commit.Sha) && window.holds(commit.Commit.Committer.Date) {
				kept = append(kept, commit)
			}
		}
		if len(batch) < g.pageSize || len(kept) >= limit {
			break
		}
	}
	if len(kept) > limit {
		kept = kept[:limit]
	}
	return kept, nil
}

// detail reads one commit's line counts and changed files. A long file list
// arrives in pages, each repeating the commit itself.
func (g *GitHub) detail(ctx context.Context, repo githubRepo, sha string) (githubCommit, *apiFailure) {
	var out githubCommit
	for page := 1; page <= 10; page++ { // the api stops listing a commit's files at 3000
		var part githubCommit
		reply, failure := g.getJson(ctx, repo.path()+"/commits/"+sha, url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}, &part)
		if failure != nil {
			return out, failure
		}
		if reply.status != http.StatusOK {
			return out, &apiFailure{reason: "commit_unreadable"} // listed a moment ago: the branch moved under the capture
		}
		if page == 1 {
			out = part
		} else {
			out.Files = append(out.Files, part.Files...)
		}
		if !strings.Contains(reply.header.Get("link"), `rel="next"`) {
			break
		}
	}
	return out, nil
}

type githubTree struct {
	// read is false when the branch has no listing to give
	read      bool
	truncated bool
	files     []File
	// paths is every entry ls-tree -r would name: blobs and submodules
	paths []string
}

func (g *GitHub) tree(ctx context.Context, repoPath, sha string) (githubTree, *apiFailure) {
	var listing struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Sha  string `json:"sha"`
			Size *int64 `json:"size"`
		} `json:"tree"`
	}
	reply, failure := g.getJson(ctx, repoPath+"/git/trees/"+sha, url.Values{"recursive": {"1"}}, &listing)
	if failure != nil {
		return githubTree{}, failure
	}
	if reply.status != http.StatusOK {
		return githubTree{}, nil
	}
	out := githubTree{read: true, truncated: listing.Truncated}
	for _, entry := range listing.Tree {
		if entry.Path == "" || entry.Type == "tree" {
			continue
		}
		out.paths = append(out.paths, entry.Path)
		if entry.Type != "blob" {
			continue
		}
		bytes := UnknownBytes
		if entry.Size != nil {
			bytes = *entry.Size
		}
		out.files = append(out.files, File{Path: entry.Path, Blob: entry.Sha, Bytes: bytes})
	}
	return out, nil
}

// content reads one file's bytes at a commit, up to the preview size. The raw
// form is asked for; a proxy that hands back the JSON form instead (a cache
// shared with a caller who asked for it) is decoded rather than shown as source.
func (g *GitHub) content(ctx context.Context, repoPath, filePath, sha string) (content string, more, found bool, failure *apiFailure) {
	segments := strings.Split(filePath, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	reply, failure := g.get(ctx, repoPath+"/contents/"+strings.Join(segments, "/"), url.Values{"ref": {sha}},
		"application/vnd.github.raw+json", 2<<20) // room for the JSON form of a file at the preview cap
	if failure != nil {
		return "", false, false, failure
	}
	if reply.status != http.StatusOK {
		return "", false, false, nil
	}
	var wrapped struct {
		Sha      string `json:"sha"`
		Url      string `json:"url"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if json.Unmarshal(reply.body, &wrapped) == nil && wrapped.Encoding == "base64" && len(wrapped.Sha) == 40 && strings.Contains(wrapped.Url, "/contents/") {
		if wrapped.Content == "" {
			return "", false, false, &apiFailure{reason: "content_not_inline"} // the JSON form leaves large files out
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(wrapped.Content, "\n", ""))
		if err != nil {
			return "", false, false, &apiFailure{reason: "bad_response"}
		}
		return string(decoded), false, true, nil
	}
	return string(reply.body), reply.more, true, nil
}

// githubBackend reads a GitHub repository through the api, with no clone
type githubBackend struct {
	api     *GitHub
	fetcher *Fetcher
}

// the outcome a clone of a missing or private repository reaches: retried through
// the capture's window, then rejected as inaccessible
const repoNotFound = "clone_failed: GitHub has no public repository at this address"

func (b githubBackend) commits(ctx context.Context, ref repoRef, window Window) (Result, *apiFailure) {
	ctx, cancel := context.WithTimeout(ctx, b.fetcher.cloneTimeout)
	defer cancel()
	api, limit := b.api, b.fetcher.maxCommits

	repo, found, failure := api.repo(ctx, ref)
	if failure != nil {
		return Result{}, failure
	}
	if !found {
		return Result{Transient: true, Error: repoNotFound, Source: "github"}, nil
	}
	head, failure := api.head(ctx, repo.path())
	if failure != nil {
		return Result{}, failure
	}
	if !head.found {
		return Result{Transient: true, Error: repoNotFound, Source: "github"}, nil
	}
	if head.sha == "" {
		return Result{Error: "log_failed: the repository has no commits", Source: "github"}, nil
	}
	listed, failure := api.list(ctx, repo, head.sha, window, limit)
	if failure != nil {
		return Result{}, failure
	}
	var later []githubCommit
	if !window.Until.IsZero() {
		// commits pushed after the ship still say which paths this repository held
		later, failure = api.list(ctx, repo, head.sha, Window{Since: window.Until.Add(time.Millisecond)}, limit)
		if failure != nil {
			return Result{}, failure
		}
	}
	tree, failure := api.tree(ctx, repo.path(), head.sha)
	if failure != nil {
		return Result{}, failure
	}

	// a merge is listed without a diff of its own, the way git log shows it
	var plain []githubCommit
	for _, commit := range append(append([]githubCommit{}, listed...), later...) {
		if len(commit.Parents) < 2 {
			plain = append(plain, commit)
		}
	}
	type detailed struct {
		sha     string
		commit  githubCommit
		failure *apiFailure
	}
	details := map[string]githubCommit{}
	for _, read := range httpx.MapLimit(ctx, plain, 6, func(ctx context.Context, commit githubCommit, _ int) detailed { // several in flight, each one released by the shared limiter
		full, failure := api.detail(ctx, repo, commit.Sha)
		return detailed{sha: commit.Sha, commit: full, failure: failure}
	}) {
		if read.failure != nil {
			return Result{}, read.failure
		}
		details[read.sha] = read.commit
	}

	build := func(source []githubCommit) []Commit {
		var out []Commit
		for _, listing := range source {
			message := listing.Commit.Message
			commit := newCommit(listing.Sha, subjectOf(message), listing.Commit.Author.Name, listing.Commit.Author.Email,
				coAuthorTrailers(message), ref.webUrl, listing.Commit.Committer.Date)
			commit.LineStats = true
			if detail, isPlain := details[listing.Sha]; isPlain {
				commit.Additions, commit.Deletions = detail.Stats.Additions, detail.Stats.Deletions
				for _, file := range detail.Files {
					if file.PreviousFilename != "" {
						commit.Paths = append(commit.Paths, file.PreviousFilename)
					}
					commit.Paths = append(commit.Paths, file.Filename)
				}
			}
			out = append(out, commit)
		}
		return out
	}
	commits := build(listed)
	out := Result{OK: true, Commits: commits, HistoryPaths: historyPaths(commits, build(later)), Source: "github"}
	if head.total > len(commits) {
		out.OutsideWindow = head.total - len(commits)
	}
	if tree.read && !tree.truncated {
		out.TreeRead = true
		out.Files = tree.files
		out.Readme = FindReadme(tree.paths)
	}
	if len(commits) >= limit {
		out.Error = "truncated"
	}
	return out, nil
}

func (b githubBackend) file(ctx context.Context, ref repoRef, filePath string) (FileContent, *apiFailure) {
	ctx, cancel := context.WithTimeout(ctx, b.fetcher.readTimeout())
	defer cancel()
	repoPath := "/repos/" + ref.owner + "/" + ref.name
	head, failure := b.api.head(ctx, repoPath)
	if failure != nil {
		return FileContent{}, failure
	}
	if head.sha == "" {
		return FileContent{Error: "read_failed: the repository has no default branch to read"}, nil
	}
	content, more, found, failure := b.api.content(ctx, repoPath, filePath, head.sha)
	if failure != nil {
		return FileContent{}, failure
	}
	if !found {
		return FileContent{Error: "read_failed: the file is not on the default branch"}, nil
	}
	return previewOf(content, more), nil
}

func (b githubBackend) repoContext(ctx context.Context, ref repoRef) (RepoContext, *apiFailure) {
	ctx, cancel := context.WithTimeout(ctx, b.fetcher.readTimeout())
	defer cancel()
	api, repoPath := b.api, "/repos/"+ref.owner+"/"+ref.name

	head, failure := api.head(ctx, repoPath)
	if failure != nil {
		return RepoContext{}, failure
	}
	if !head.found {
		return RepoContext{Error: repoNotFound}, nil
	}
	if head.sha == "" {
		return RepoContext{Error: "tree_failed: the default branch has no file listing"}, nil
	}
	tree, failure := api.tree(ctx, repoPath, head.sha)
	if failure != nil {
		return RepoContext{}, failure
	}
	if !tree.read {
		return RepoContext{Error: "tree_failed: the default branch has no file listing"}, nil
	}
	out := RepoContext{OK: true, Paths: tree.paths, PathsTruncated: tree.truncated, ReadmeName: FindReadme(tree.paths)}
	if out.ReadmeName == "" && tree.truncated {
		// a listing cut short can end before the root files: ask for the root alone
		var root []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		}
		if _, failure := api.getJson(ctx, repoPath+"/contents", url.Values{"ref": {head.sha}}, &root); failure != nil {
			return RepoContext{}, failure
		}
		var names []string
		for _, entry := range root {
			if entry.Type == "file" {
				names = append(names, entry.Name)
			}
		}
		out.ReadmeName = FindReadme(names)
	}
	if out.ReadmeName == "" {
		return out, nil
	}
	content, more, found, failure := api.content(ctx, repoPath, out.ReadmeName, head.sha)
	if failure != nil {
		return RepoContext{}, failure
	}
	if !found {
		out.Error = "readme_failed: the README could not be read"
		return out, nil
	}
	out.Readme, out.ReadmeTruncated = content, more
	if len(out.Readme) > MaxPreviewBytes {
		out.Readme, out.ReadmeTruncated = out.Readme[:MaxPreviewBytes], true
	}
	return out, nil
}
