package testgit

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// GitHubFake is a GitHub api proxy in front of a fake GitHub: it wants an api
// key, speaks its own error envelope, advertises and can enforce a per-key rate,
// and forwards everything else GitHub answers untouched. GitHub's side is served
// from a real bare repository, so a capture through the api and a clone of the
// same repository can be compared.
type GitHubFake struct {
	// URL is the proxy's base; GitHub's paths live under URL + "/gh"
	URL string
	// Key is the api key the proxy accepts
	Key string

	// KeyError, when set, is the proxy error every request is refused with
	// (INVALID_API_KEY, API_KEY_DISABLED)
	KeyError string
	// ProxyError, when set, is a proxy error answered in place of forwarding
	// (NOT_FOUND, METHOD_NOT_ALLOWED, REQUEST_TOO_LARGE)
	ProxyError string
	// DbErrors is how many of the next requests answer the proxy's 503 DB_ERROR
	DbErrors int
	// Throttled is how many of the next requests answer the proxy's 429
	Throttled int
	// Quota, when set, is advertised and enforced: more requests than this inside
	// one second are refused with 429 and counted in Refused
	Quota int
	// Cached serves a repeated url from the first successful answer, as the proxy's cache does
	Cached bool

	// RenamedFrom is an old "owner/name" GitHub redirects to the repository
	RenamedFrom string
	// Limited is how many of the next requests GitHub answers as rate limited
	Limited int
	// Broken is how many of the next requests GitHub answers 502
	Broken int
	// Status, when set, is a bare status answered with a body in no known shape
	Status int
	// TruncatedTree cuts the file listing to its first entry and says so
	TruncatedTree bool
	// FilesPerPage pages a commit's file list, 0 for one page
	FilesPerPage int
	// JsonContents answers file reads in the JSON form whatever form was asked for
	JsonContents bool

	t        *testing.T
	bare     string
	fullName string
	mu       sync.Mutex
	requests []string
	keyless  int
	refused  int
	arrivals []time.Time
	cache    map[string]*httptest.ResponseRecorder
}

// ServeGitHub serves the bare repository as fullName ("owner/name").
func ServeGitHub(t *testing.T, bare, fullName string) *GitHubFake {
	t.Helper()
	fake := &GitHubFake{t: t, bare: bare, fullName: fullName, Key: "key1", cache: map[string]*httptest.ResponseRecorder{}}
	srv := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(srv.Close)
	fake.URL = srv.URL
	return fake
}

// Requests is every request served so far, as "path?query".
func (f *GitHubFake) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.requests...)
}

// Keyless is how many requests arrived without an api key.
func (f *GitHubFake) Keyless() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keyless
}

// Refused is how many requests the enforced Quota turned away.
func (f *GitHubFake) Refused() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refused
}

func (f *GitHubFake) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = f.bare
	out, err := cmd.Output()
	return string(out), err
}

func answer(w http.ResponseWriter, status int, body any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func notFound(w http.ResponseWriter) {
	answer(w, http.StatusNotFound, map[string]string{"message": "Not Found", "documentation_url": "https://docs.github.com/rest"})
}

var proxyStatuses = map[string]int{
	"MISSING_API_KEY": 401, "INVALID_API_KEY": 401, "API_KEY_DISABLED": 403, "NOT_FOUND": 404,
	"METHOD_NOT_ALLOWED": 405, "REQUEST_TOO_LARGE": 413, "RATE_LIMIT_EXCEEDED": 429, "DB_ERROR": 503,
}

func proxyError(w http.ResponseWriter, code string) {
	answer(w, proxyStatuses[code], map[string]any{"error": map[string]string{
		"code": code, "message": "refused by the proxy", "hint": "see the documentation", "documentation_url": "https://gh-proxy.example.test/docs",
	}})
}

// serve is the proxy: its own checks first, then GitHub's answer forwarded.
func (f *GitHubFake) serve(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("x-api-key")
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.RequestURI())
	if key == "" {
		f.keyless++
	}
	keyError, proxyFault := f.KeyError, f.ProxyError
	dbError := f.DbErrors > 0
	if dbError {
		f.DbErrors--
	}
	quota := 1000 // roomy unless a test sets Quota, so only tests about the allowance wait on it
	throttled := false
	if key == f.Key && keyError == "" {
		if f.Throttled > 0 {
			f.Throttled--
			throttled = true
		}
		if f.Quota > 0 {
			quota = f.Quota
			now := time.Now()
			recent := f.arrivals[:0]
			for _, at := range f.arrivals {
				if now.Sub(at) < time.Second {
					recent = append(recent, at)
				}
			}
			f.arrivals = recent
			if len(recent) >= quota {
				f.refused++
				throttled = true
			} else {
				f.arrivals = append(f.arrivals, now)
			}
		}
	}
	remaining := max(quota-len(f.arrivals), 0)
	if f.Quota == 0 {
		remaining = quota - 1
	}
	cached := f.cache[r.URL.RequestURI()]
	if !f.Cached {
		cached = nil
	}
	f.mu.Unlock()

	rest, isGitHub := strings.CutPrefix(r.URL.Path, "/gh/")
	switch {
	case !isGitHub:
		proxyError(w, "NOT_FOUND")
		return
	case r.Method != http.MethodGet:
		proxyError(w, "METHOD_NOT_ALLOWED")
		return
	case key == "":
		proxyError(w, "MISSING_API_KEY")
		return
	case keyError != "":
		proxyError(w, keyError)
		return
	case key != f.Key:
		proxyError(w, "INVALID_API_KEY")
		return
	case dbError:
		proxyError(w, "DB_ERROR")
		return
	}
	w.Header().Set("ratelimit-limit", strconv.Itoa(quota))
	w.Header().Set("ratelimit-remaining", strconv.Itoa(remaining))
	w.Header().Set("ratelimit-reset", "1")
	w.Header().Set("ratelimit-policy", fmt.Sprintf(`"default";q=%d;w=1`, quota))
	if throttled {
		w.Header().Set("retry-after", "1")
		proxyError(w, "RATE_LIMIT_EXCEEDED")
		return
	}
	if proxyFault != "" {
		proxyError(w, proxyFault)
		return
	}

	state := "miss"
	if cached != nil {
		state = "hit"
	} else {
		cached = httptest.NewRecorder()
		f.github(cached, r, "/"+rest)
		if f.Cached && cached.Code == http.StatusOK {
			f.mu.Lock()
			f.cache[r.URL.RequestURI()] = cached
			f.mu.Unlock()
		}
	}
	for name, values := range cached.Header() {
		w.Header()[name] = values
	}
	w.Header().Set("x-gh-proxy-cache", state)
	w.Header().Set("x-gh-proxy-category", "core")
	w.WriteHeader(cached.Code)
	w.Write(cached.Body.Bytes())
}

// github is api.github.com: path is what GitHub itself would be asked for.
func (f *GitHubFake) github(w http.ResponseWriter, r *http.Request, path string) {
	f.mu.Lock()
	status := f.Status
	limited, broken := f.Limited > 0, f.Broken > 0
	if limited {
		f.Limited--
	} else if broken {
		f.Broken--
	}
	f.mu.Unlock()

	if limited {
		w.Header().Set("x-ratelimit-remaining", "0")
		w.Header().Set("x-ratelimit-reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		answer(w, http.StatusForbidden, map[string]string{"message": "API rate limit exceeded"})
		return
	}
	if broken {
		answer(w, http.StatusBadGateway, map[string]string{"message": "Server Error"})
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		w.Write([]byte("<html>not an api</html>"))
		return
	}

	rest := ""
	switch {
	case path == "/repositories/1" || strings.HasPrefix(path, "/repositories/1/"):
		rest = strings.TrimPrefix(path, "/repositories/1")
	case f.RenamedFrom != "" && (path == "/repos/"+f.RenamedFrom || strings.HasPrefix(path, "/repos/"+f.RenamedFrom+"/")):
		target := "https://api.github.com/repositories/1" + strings.TrimPrefix(path, "/repos/"+f.RenamedFrom)
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		w.Header().Set("location", target)
		answer(w, http.StatusMovedPermanently, map[string]string{"message": "Moved Permanently", "url": target})
		return
	case path == "/repos/"+f.fullName || strings.HasPrefix(path, "/repos/"+f.fullName+"/"):
		rest = strings.TrimPrefix(path, "/repos/"+f.fullName)
	default:
		notFound(w)
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = "HEAD"
	}

	switch {
	case rest == "":
		answer(w, http.StatusOK, map[string]any{"full_name": f.fullName})
	case rest == "/commits":
		f.listCommits(w, r)
	case strings.HasPrefix(rest, "/commits/"):
		f.commitDetail(w, r, strings.TrimPrefix(rest, "/commits/"))
	case strings.HasPrefix(rest, "/git/trees/"):
		f.tree(w, strings.TrimPrefix(rest, "/git/trees/"))
	case rest == "/contents":
		f.rootListing(w, ref)
	case strings.HasPrefix(rest, "/contents/"):
		filePath := strings.TrimPrefix(rest, "/contents/")
		content, err := f.git("cat-file", "blob", ref+":"+filePath)
		if err != nil {
			notFound(w)
			return
		}
		if f.JsonContents {
			blob, _ := f.git("rev-parse", ref+":"+filePath)
			encoded := base64.StdEncoding.EncodeToString([]byte(content))
			var lines []string
			for len(encoded) > 60 {
				lines, encoded = append(lines, encoded[:60]), encoded[60:]
			}
			answer(w, http.StatusOK, map[string]string{
				"type": "file", "path": filePath, "sha": strings.TrimSpace(blob), "encoding": "base64",
				"url": "https://api.github.com/repos/" + f.fullName + "/contents/" + filePath, "content": strings.Join(append(lines, encoded), "\n") + "\n",
			})
			return
		}
		w.Header().Set("content-type", "application/vnd.github.raw+json")
		w.Write([]byte(content))
	default:
		notFound(w)
	}
}

type fakeCommit struct {
	sha, message, authorName, authorEmail string
	parents                               []string
	committedAt                           time.Time
}

func (c fakeCommit) json() map[string]any {
	parents := []map[string]string{}
	for _, parent := range c.parents {
		parents = append(parents, map[string]string{"sha": parent})
	}
	return map[string]any{
		"sha": c.sha,
		"commit": map[string]any{
			"message":   c.message,
			"author":    map[string]string{"name": c.authorName, "email": c.authorEmail},
			"committer": map[string]string{"date": c.committedAt.UTC().Format(time.RFC3339)},
		},
		"parents": parents,
	}
}

func (f *GitHubFake) history(ref string) ([]fakeCommit, bool) {
	out, err := f.git("log", ref, "--format=%H%x1f%P%x1f%an%x1f%ae%x1f%cI%x1f%B%x1e")
	if err != nil {
		return nil, false
	}
	var commits []fakeCommit
	for _, record := range strings.Split(out, "\x1e") {
		fields := strings.SplitN(strings.TrimLeft(record, "\n"), "\x1f", 6)
		if len(fields) != 6 {
			continue
		}
		committedAt, err := time.Parse(time.RFC3339, fields[4])
		if err != nil {
			f.t.Errorf("fake github: commit date %q", fields[4])
		}
		commits = append(commits, fakeCommit{
			sha: fields[0], parents: strings.Fields(fields[1]), authorName: fields[2], authorEmail: fields[3],
			committedAt: committedAt, message: strings.TrimSuffix(fields[5], "\n"),
		})
	}
	return commits, true
}

func page(r *http.Request, size, total int) (from, to int, link string) {
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if size > 0 {
		perPage = size
	}
	if perPage < 1 {
		perPage = 30
	}
	number, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if number < 1 {
		number = 1
	}
	from = min((number-1)*perPage, total)
	to = min(from+perPage, total)
	if to < total {
		query := r.URL.Query()
		query.Set("page", strconv.Itoa(number+1))
		github := "https://api.github.com" + strings.TrimPrefix(r.URL.Path, "/gh") // GitHub names its own host, as the proxy forwards it
		link = fmt.Sprintf(`<%s?%s>; rel="next", `, github, query.Encode())
		query.Set("page", strconv.Itoa((total+perPage-1)/perPage))
		link += fmt.Sprintf(`<%s?%s>; rel="last"`, github, query.Encode())
	}
	return from, to, link
}

func (f *GitHubFake) listCommits(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("sha")
	if ref == "" {
		ref = "HEAD"
	}
	all, ok := f.history(ref)
	if !ok {
		answer(w, http.StatusConflict, map[string]string{"message": "Git Repository is empty."})
		return
	}
	bound := func(name string) (time.Time, bool) {
		raw := r.URL.Query().Get(name)
		if raw == "" {
			return time.Time{}, false
		}
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			f.t.Errorf("fake github: %s=%q is not a timestamp", name, raw)
		}
		return at, true
	}
	since, hasSince := bound("since")
	until, hasUntil := bound("until")
	matched := []map[string]any{}
	for _, commit := range all {
		if (hasSince && commit.committedAt.Before(since)) || (hasUntil && commit.committedAt.After(until)) {
			continue
		}
		matched = append(matched, commit.json())
	}
	from, to, link := page(r, 0, len(matched))
	if link != "" {
		w.Header().Set("link", link)
	}
	answer(w, http.StatusOK, matched[from:to])
}

func (f *GitHubFake) commitDetail(w http.ResponseWriter, r *http.Request, sha string) {
	all, _ := f.history(sha)
	if len(all) == 0 || all[0].sha != sha {
		notFound(w)
		return
	}
	commit := all[0]
	if len(commit.parents) > 1 {
		f.t.Errorf("fake github: asked for the diff of merge %s, which git log never shows", sha)
	}
	stats, _ := f.git("show", "--format=", "--numstat", "-M", sha)
	additions, deletions := 0, 0
	for _, line := range strings.Split(stats, "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		added, _ := strconv.Atoi(fields[0])
		removed, _ := strconv.Atoi(fields[1])
		additions += added
		deletions += removed
	}
	names, _ := f.git("show", "--format=", "--name-status", "-M", "-z", sha)
	fields := strings.Split(strings.TrimLeft(names, "\n"), "\x00")
	files := []map[string]string{}
	for i := 0; i+1 < len(fields); i += 2 {
		if strings.HasPrefix(fields[i], "R") && i+2 < len(fields) {
			files = append(files, map[string]string{"filename": fields[i+2], "previous_filename": fields[i+1], "status": "renamed"})
			i++
			continue
		}
		files = append(files, map[string]string{"filename": fields[i+1], "status": "modified"})
	}
	from, to, link := page(r, f.FilesPerPage, len(files))
	if f.FilesPerPage == 0 {
		from, to, link = 0, len(files), ""
	}
	if link != "" {
		w.Header().Set("link", link)
	}
	body := commit.json()
	body["stats"] = map[string]int{"additions": additions, "deletions": deletions, "total": additions + deletions}
	body["files"] = files[from:to]
	answer(w, http.StatusOK, body)
}

func (f *GitHubFake) tree(w http.ResponseWriter, ref string) {
	out, err := f.git("ls-tree", "-r", "--long", "-z", ref)
	if err != nil {
		answer(w, http.StatusConflict, map[string]string{"message": "Git Repository is empty."})
		return
	}
	entries := []map[string]any{}
	for _, record := range strings.Split(out, "\x00") {
		meta, path, hasPath := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !hasPath || len(fields) != 4 {
			continue
		}
		entry := map[string]any{"path": path, "mode": fields[0], "type": fields[1], "sha": fields[2]}
		if size, err := strconv.ParseInt(fields[3], 10, 64); err == nil {
			entry["size"] = size
		}
		entries = append(entries, entry)
	}
	if f.TruncatedTree && len(entries) > 1 {
		entries = entries[:1]
	}
	answer(w, http.StatusOK, map[string]any{"sha": ref, "tree": entries, "truncated": f.TruncatedTree})
}

func (f *GitHubFake) rootListing(w http.ResponseWriter, ref string) {
	out, err := f.git("ls-tree", "-z", ref)
	if err != nil {
		notFound(w)
		return
	}
	entries := []map[string]string{}
	for _, record := range strings.Split(out, "\x00") {
		meta, name, hasName := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !hasName || len(fields) != 3 {
			continue
		}
		kind := "file"
		if fields[1] == "tree" {
			kind = "dir"
		}
		entries = append(entries, map[string]string{"name": name, "path": name, "type": kind})
	}
	answer(w, http.StatusOK, entries)
}
