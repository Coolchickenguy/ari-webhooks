package githost

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hackclub/ari-webhooks/internal/testgit"
)

// apiFixture is the history repository behind a fake GitHub proxy and a real git
// server, with a fetcher that treats the git server's host as GitHub.
type apiFixture struct {
	fake    *testgit.GitHubFake
	fetcher *Fetcher
	repoUrl string
	bare    string
	// pauses is every wait the client asked for; none of them is actually slept
	pauses *[]time.Duration
}

// proxyOnly fails any request that is not addressed to the configured proxy:
// nothing may reach GitHub's own host, whatever a Link or Location header names.
type proxyOnly struct {
	t      *testing.T
	github *GitHub
}

func (p proxyOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.HasPrefix(req.URL.String(), p.github.proxyUrl+"/gh/") {
		p.t.Errorf("request left the proxy: %s", req.URL)
		return nil, errors.New("request left the proxy")
	}
	if req.Header.Get("x-api-key") == "" || req.Header.Get("authorization") != "" {
		p.t.Errorf("request must carry the proxy key and no token: %s", req.URL)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func proxied(t *testing.T, fake *testgit.GitHubFake, host string) (*Fetcher, *[]time.Duration) {
	t.Helper()
	f := localFetcher()
	f.GitHub = NewGitHub(fake.URL, fake.Key, host)
	f.GitHub.client.Transport = proxyOnly{t: t, github: f.GitHub}
	pauses := &[]time.Duration{}
	var mu sync.Mutex
	clock := time.Now()
	f.GitHub.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	f.GitHub.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(d) // time passes without the test waiting for it
		*pauses = append(*pauses, d)
		return nil
	}
	t.Cleanup(func() {
		if fake.Keyless() != 0 {
			t.Errorf("%d requests reached the proxy without a key", fake.Keyless())
		}
	})
	return f, pauses
}

func setupApi(t *testing.T) apiFixture {
	t.Helper()
	srv, bare, repoUrl := serveHistory(t, buildHistory(t))
	fake := testgit.ServeGitHub(t, bare, "owner1/project1")
	f, pauses := proxied(t, fake, strings.TrimPrefix(srv.URL, "http://"))
	return apiFixture{fake: fake, fetcher: f, repoUrl: repoUrl, bare: bare, pauses: pauses}
}

// apiOnly points the fetcher at a host nothing listens on, so any answer has to
// have come from the api: a clone would fail.
func apiOnly(t *testing.T, src string) apiFixture {
	t.Helper()
	_, bare, _ := serveHistory(t, src)
	fake := testgit.ServeGitHub(t, bare, "owner1/project1")
	f, pauses := proxied(t, fake, "127.0.0.1:9")
	return apiFixture{fake: fake, fetcher: f, repoUrl: "http://127.0.0.1:9/owner1/project1", bare: bare, pauses: pauses}
}

func requestsMatching(fake *testgit.GitHubFake, part string) int {
	count := 0
	for _, request := range fake.Requests() {
		if strings.Contains(request, part) {
			count++
		}
	}
	return count
}

func TestGitHubRepoUrlShapes(t *testing.T) {
	github := NewGitHub("", "", "")
	for raw, want := range map[string]string{
		"https://github.com/owner1/project1":                 "owner1/project1",
		"https://github.com/owner1/project1.git":             "owner1/project1",
		"https://github.com/owner1/project1/":                "owner1/project1",
		"https://www.github.com/owner1/project1":             "owner1/project1",
		"https://GitHub.com/Owner1/Project.One":              "Owner1/Project.One",
		"https://github.com/owner1/project1/tree/main/src":   "owner1/project1",
		"https://github.com/owner1/project1/blob/main/a.txt": "owner1/project1",
		"https://github.com/owner1":                          "",
		"https://gitlab.com/owner1/project1":                 "",
		"https://gist.github.com/owner1/project1":            "",
		"https://user1:secret@github.com/owner1/project1":    "",
		"https://github.com/owner1/..":                       "",
		"https://github.com/owner1/pro~ject1":                "",
		"https://github.com/owner 1/project1":                "",
	} {
		ref, ok := github.repoRef(raw)
		got := ""
		if ok {
			got = ref.owner + "/" + ref.name
		}
		if got != want {
			t.Fatalf("%s: got %q want %q", raw, got, want)
		}
		if ok && (ref.cloneUrl != "https://github.com/"+want || ref.webUrl != ref.cloneUrl) {
			t.Fatalf("%s: clone url %q", raw, ref.cloneUrl)
		}
	}

	enterprise := NewGitHub("https://gh-proxy.example.test", "key1", "git.example.com")
	if ref, ok := enterprise.repoRef("https://git.example.com/owner1/project1.git"); !ok || ref.name != "project1" {
		t.Fatalf("a configured host is read as GitHub: %+v", ref)
	}
	if _, ok := enterprise.repoRef("https://github.com/owner1/project1"); ok {
		t.Fatal("only the configured host goes through its api")
	}
}

func TestGitHubReadsCommitsWithoutCloning(t *testing.T) {
	fx := apiOnly(t, buildHistory(t))
	res := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
	if !res.OK || res.Error != "" || res.Source != "github" || len(res.Notes) != 0 {
		t.Fatalf("fetch: %+v", res)
	}
	want := []string{"delete notes", "merge side", "a title on two lines that git folds into one", "side work", "rename a"}
	if !equal(subjects(res.Commits), want) {
		t.Fatalf("commits in the window, newest first:\n got %q\nwant %q", subjects(res.Commits), want)
	}
	if res.OutsideWindow != 2 {
		t.Fatalf("outside the window: %d", res.OutsideWindow)
	}
	byMessage := map[string]Commit{}
	for _, commit := range res.Commits {
		if !commit.LineStats {
			t.Fatalf("the api counts lines: %+v", commit)
		}
		byMessage[commit.Message] = commit
	}
	if c := byMessage["rename a"]; c.Additions != 1 || c.Deletions != 0 || !equal(sorted(c.Paths), []string{"src/a.txt", "src/b.txt"}) {
		t.Fatalf("rename: +%d -%d %q", c.Additions, c.Deletions, c.Paths)
	}
	if c := byMessage["delete notes"]; c.Additions != 0 || c.Deletions != 1 || !equal(c.Paths, []string{"docs/with space.md"}) {
		t.Fatalf("delete: +%d -%d %q", c.Additions, c.Deletions, c.Paths)
	}
	if c := byMessage["merge side"]; c.Additions != 0 || c.Deletions != 0 || len(c.Paths) != 0 {
		t.Fatalf("a merge is listed with no diff of its own, as git log shows it: %+v", c)
	}
	titled := byMessage["a title on two lines that git folds into one"]
	if len(titled.CoAuthors) != 2 || *titled.CoAuthors[0].Name != "User Two" || *titled.CoAuthors[0].Email != "user2@example.com" ||
		titled.CoAuthors[1].Name != nil || *titled.CoAuthors[1].Email != "user3@example.com" {
		t.Fatalf("co-authors come from the trailer block of the full message: %+v", titled.CoAuthors)
	}
	if titled.AuthorName != "User One" || titled.AuthorEmail != "user1@example.com" || !titled.CommittedAt.Equal(at(4)) {
		t.Fatalf("author and time: %+v", titled)
	}
	if titled.Url != "http://127.0.0.1:9/owner1/project1/commit/"+titled.Hash || titled.ShortHash != titled.Hash[:7] {
		t.Fatalf("commit link: %s", titled.Url)
	}
	if got := sorted(res.HistoryPaths); !equal(got, []string{"docs/with space.md", "later.txt", "main.txt", "side.txt", "src/a.txt", "src/b.txt"}) {
		t.Fatalf("history paths: %q", got)
	}

	if !res.TreeRead || res.Readme != "README.md" || len(res.Files) != 5 {
		t.Fatalf("tree: read=%v readme=%q files=%+v", res.TreeRead, res.Readme, res.Files)
	}
	for _, file := range res.Files {
		blob, err := exec.Command("git", "-C", fx.bare, "rev-parse", "HEAD:"+file.Path).Output()
		if err != nil || strings.TrimSpace(string(blob)) != file.Blob {
			t.Fatalf("%s: blob id %q, git says %q", file.Path, file.Blob, blob)
		}
		if file.Path == "main.txt" && file.Bytes != 10 {
			t.Fatalf("the api gives sizes: %+v", file)
		}
	}
	head, _ := exec.Command("git", "-C", fx.bare, "rev-parse", "trunk").Output()
	sha := strings.TrimSpace(string(head))
	if requestsMatching(fx.fake, "/git/trees/"+sha) != 1 || requestsMatching(fx.fake, "sha="+sha) == 0 {
		t.Fatalf("the tree and the list are read at the head of the default branch, here trunk: %q", fx.fake.Requests())
	}
	for _, request := range fx.fake.Requests() {
		if !strings.HasPrefix(request, "/gh/repos/owner1/project1") {
			t.Fatalf("every request goes under the proxy's /gh/ prefix: %s", request)
		}
	}
}

func TestGitHubPagesUntilTheListEnds(t *testing.T) {
	fx := apiOnly(t, buildHistory(t))
	fx.fetcher.GitHub.pageSize = 2
	fx.fake.FilesPerPage = 1
	res := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, Window{})
	if !res.OK || len(res.Commits) != 7 || res.OutsideWindow != 0 {
		t.Fatalf("seven commits over four pages: %+v", res)
	}
	if requestsMatching(fx.fake, "per_page=2&sha=") != 4 {
		t.Fatalf("pages asked for: %q", fx.fake.Requests())
	}
	for _, commit := range res.Commits {
		if commit.Message == "first" && !equal(sorted(commit.Paths), []string{"README.md", "docs/with space.md", "src/a.txt"}) {
			t.Fatalf("a commit's file list is followed across its pages: %q", commit.Paths)
		}
	}
}

func TestGitHubWindowBoundsAreExact(t *testing.T) {
	fx := apiOnly(t, buildHistory(t))
	justInside := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, Window{Since: at(2).Add(-999 * time.Millisecond), Until: at(6).Add(999 * time.Millisecond)})
	if len(justInside.Commits) != 5 {
		t.Fatalf("a bound within the same second keeps the edge commits: %q", subjects(justInside.Commits))
	}
	justOutside := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, Window{Since: at(2).Add(time.Millisecond), Until: at(6).Add(-time.Millisecond)})
	if !equal(subjects(justOutside.Commits), []string{"merge side", "a title on two lines that git folds into one", "side work"}) {
		t.Fatalf("a millisecond inside each edge drops both edge commits: %q", subjects(justOutside.Commits))
	}
	if requestsMatching(fx.fake, "since=") == 0 || requestsMatching(fx.fake, "until=") == 0 {
		t.Fatalf("the window is sent to the api: %q", fx.fake.Requests())
	}
}

func TestGitHubTruncatesAtTheCapInsideTheWindow(t *testing.T) {
	fx := apiOnly(t, buildHistory(t))
	fx.fetcher.maxCommits = 3
	fx.fetcher.GitHub.pageSize = 2
	res := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
	if !res.OK || res.Error != "truncated" {
		t.Fatalf("a capped history still succeeds and says so: %+v", res)
	}
	if !equal(subjects(res.Commits), []string{"delete notes", "merge side", "a title on two lines that git folds into one"}) {
		t.Fatalf("the cap keeps the newest commits of the window: %q", subjects(res.Commits))
	}
}

func TestGitHubFollowsARenamedRepository(t *testing.T) {
	fx := apiOnly(t, buildHistory(t))
	fx.fake.RenamedFrom = "owner1/oldname"
	res := fx.fetcher.FetchCommits(context.Background(), "http://127.0.0.1:9/owner1/oldname", shipWindow)
	if !res.OK || len(res.Commits) != 5 || res.Source != "github" {
		t.Fatalf("a renamed repository is read under its new name: %+v", res)
	}
	if requestsMatching(fx.fake, "/gh/repos/owner1/oldname") != 1 || requestsMatching(fx.fake, "/gh/repositories/1") != 1 {
		t.Fatalf("the redirect is taken through the proxy once, then the name GitHub answered with is used: %q", fx.fake.Requests())
	}
	if !strings.HasPrefix(res.Commits[0].Url, "http://127.0.0.1:9/owner1/oldname/commit/") {
		t.Fatalf("commit links keep the address the ship gave: %s", res.Commits[0].Url)
	}
}

// A missing or private repository answers 404. That is the repository's own
// answer, so it takes the path a failed clone takes and never falls back.
func TestGitHubMissingRepositoryIsInaccessible(t *testing.T) {
	fx := setupApi(t)
	res := fx.fetcher.FetchCommits(context.Background(), strings.Replace(fx.repoUrl, "project1", "project404", 1), shipWindow)
	if res.OK || !res.Transient || res.TimedOut || errorCode(res.Error) != "clone_failed" {
		t.Fatalf("a 404 is clone_failed, retried and then rejected like a failed clone: %+v", res)
	}
	if res.Source != "github" || len(res.Notes) != 0 {
		t.Fatalf("the api answered, so nothing was cloned: %+v", res)
	}
}

func TestGitHubEmptyRepositoryIsNotTransient(t *testing.T) {
	root := t.TempDir()
	bare := root + "/empty"
	gitAt(t, root, day(1), "init", "-q", "--bare", "-b", "main", bare)
	fake := testgit.ServeGitHub(t, bare, "owner1/empty")
	f, _ := proxied(t, fake, "127.0.0.1:9")
	res := f.FetchCommits(context.Background(), "http://127.0.0.1:9/owner1/empty", Window{})
	if res.OK || res.Transient || errorCode(res.Error) != "log_failed" || res.Source != "github" {
		t.Fatalf("a repository without commits is log_failed and final, as a clone reports it: %+v", res)
	}
}

func sameCommits(t *testing.T, got, want []Commit) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("commit count: got %d want %d", len(got), len(want))
	}
	for i := range want {
		a, b := got[i], want[i]
		if a.Hash != b.Hash || a.ShortHash != b.ShortHash || a.Message != b.Message || !a.CommittedAt.Equal(b.CommittedAt) ||
			a.AuthorName != b.AuthorName || a.AuthorEmail != b.AuthorEmail || a.Url != b.Url || !equal(sorted(a.Paths), sorted(b.Paths)) {
			t.Fatalf("commit %d differs:\n got %+v\nwant %+v", i, a, b)
		}
		if len(a.CoAuthors) != len(b.CoAuthors) {
			t.Fatalf("commit %d co-authors: got %+v want %+v", i, a.CoAuthors, b.CoAuthors)
		}
		for j := range b.CoAuthors {
			if deref(a.CoAuthors[j].Name) != deref(b.CoAuthors[j].Name) || deref(a.CoAuthors[j].Email) != deref(b.CoAuthors[j].Email) {
				t.Fatalf("commit %d co-author %d: got %+v want %+v", i, j, a.CoAuthors[j], b.CoAuthors[j])
			}
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// The same repository read both ways must give the same commits, so the two
// backends cannot drift apart.
func TestBackendsAgree(t *testing.T) {
	fx := setupApi(t)
	for name, window := range map[string]Window{
		"ship window": shipWindow,
		"open":        {},
		"since only":  {Since: at(4)},
		"until only":  {Until: at(3)},
	} {
		viaApi := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, window)
		viaGit := localFetcher().FetchCommits(context.Background(), fx.repoUrl, window)
		if !viaApi.OK || viaApi.Source != "github" || !viaGit.OK || viaGit.Source != "git" {
			t.Fatalf("%s: api %+v\ngit %+v", name, viaApi, viaGit)
		}
		sameCommits(t, viaApi.Commits, viaGit.Commits)
		if !equal(sorted(viaApi.HistoryPaths), sorted(viaGit.HistoryPaths)) {
			t.Fatalf("%s: history paths: api %q git %q", name, viaApi.HistoryPaths, viaGit.HistoryPaths)
		}
		if viaApi.OutsideWindow != viaGit.OutsideWindow || viaApi.Readme != viaGit.Readme || viaApi.TreeRead != viaGit.TreeRead || viaApi.Error != viaGit.Error {
			t.Fatalf("%s: api %+v\ngit %+v", name, viaApi, viaGit)
		}
		if len(viaApi.Files) != len(viaGit.Files) {
			t.Fatalf("%s: files: api %+v git %+v", name, viaApi.Files, viaGit.Files)
		}
		for i, file := range viaGit.Files {
			if viaApi.Files[i].Path != file.Path || viaApi.Files[i].Blob != file.Blob {
				t.Fatalf("%s: file %d: api %+v git %+v", name, i, viaApi.Files[i], file)
			}
		}
	}
}

// GitHub's own rate limit, forwarded by the proxy, is the limit of whichever
// pooled token served the request. One retry may land on another token; after
// that the capture is finished by a clone. It is never "repository missing", and
// it never keeps later captures away from the api.
func TestGitHubRateLimitRetriesThenFallsBackToAClone(t *testing.T) {
	fx := setupApi(t)
	direct := localFetcher().FetchCommits(context.Background(), fx.repoUrl, shipWindow)

	fx.fake.Limited = 1
	res := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
	if !res.OK || res.Source != "github" || len(res.Notes) != 0 {
		t.Fatalf("one limited answer is retried and the api still serves the capture: %+v", res)
	}
	if len(*fx.pauses) != 1 || (*fx.pauses)[0] < time.Second || (*fx.pauses)[0] > 2*time.Second {
		t.Fatalf("a short pause before the retry: %v", *fx.pauses)
	}

	fx.fake.Limited = 1000
	asked := len(fx.fake.Requests())
	res = fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
	if !res.OK || res.Source != "git" {
		t.Fatalf("a rate limited capture must still succeed, through a clone: %+v", res)
	}
	if len(res.Notes) != 1 || res.Notes[0] != "github_api_skipped: rate_limited" {
		t.Fatalf("the fallback is noted: %q", res.Notes)
	}
	sameCommits(t, res.Commits, direct.Commits)
	if !equal(sorted(res.HistoryPaths), sorted(direct.HistoryPaths)) || len(res.Files) != len(direct.Files) {
		t.Fatalf("the fallback result is the clone's result: %+v", res)
	}
	if got := len(fx.fake.Requests()) - asked; got != 2 {
		t.Fatalf("one request and one retry, then the clone: %d requests", got)
	}
	file := fx.fetcher.FetchFile(context.Background(), fx.repoUrl, "main.txt")
	if !file.OK || file.Content != "main\nwork\n" {
		t.Fatalf("file reads fall back the same way: %+v", file)
	}

	fx.fake.Limited = 0
	resumed := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
	if !resumed.OK || resumed.Source != "github" {
		t.Fatalf("the very next capture asks the api again, with no blackout: %+v", resumed)
	}
}

func TestGitHubRateLimitIsRecognised(t *testing.T) {
	plain := http.Header{}
	if githubRateLimited(http.StatusForbidden, plain, []byte(`{"message":"Resource not accessible by personal access token"}`)) {
		t.Fatal("a plain 403 is not a rate limit")
	}
	if !githubRateLimited(http.StatusForbidden, plain, []byte(`{"message":"You have exceeded a secondary rate limit."}`)) {
		t.Fatal("a secondary limit is named in the message")
	}
	if !githubRateLimited(http.StatusForbidden, http.Header{"Retry-After": {"30"}}, nil) || !githubRateLimited(http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"0"}}, nil) {
		t.Fatal("retry-after or a spent budget marks a limit")
	}
	if githubRateLimited(http.StatusNotFound, http.Header{"X-Ratelimit-Remaining": {"0"}}, nil) {
		t.Fatal("a 404 is an answer, whatever the donor token has left")
	}
}

func TestGitHubServerErrorRetriesThenFallsBack(t *testing.T) {
	fx := setupApi(t)
	fx.fake.Broken = 2
	res := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
	if !res.OK || res.Source != "github" || len(res.Commits) != 5 || len(*fx.pauses) != 2 {
		t.Fatalf("two 502s are retried with a pause each and the api still answers: %+v %v", res, *fx.pauses)
	}

	fx.fake.Broken = 1000
	asked := len(fx.fake.Requests())
	res = fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
	if !res.OK || res.Source != "git" || len(res.Commits) != 5 {
		t.Fatalf("a failing api must fall back to a clone: %+v", res)
	}
	if len(res.Notes) != 1 || res.Notes[0] != "github_api_skipped: http_502" || len(fx.fake.Requests())-asked != 3 {
		t.Fatalf("three tries, then the fallback is noted: %q after %d requests", res.Notes, len(fx.fake.Requests())-asked)
	}
}

// The proxy could not check the key (its 503 DB_ERROR): a passing fault.
func TestProxyDbErrorRetriesThenFallsBack(t *testing.T) {
	fx := setupApi(t)
	fx.fake.DbErrors = 2
	res := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
	if !res.OK || res.Source != "github" || len(*fx.pauses) != 2 {
		t.Fatalf("two DB_ERRORs are retried with backoff: %+v %v", res, *fx.pauses)
	}
	if (*fx.pauses)[1] <= (*fx.pauses)[0] {
		t.Fatalf("the second wait is longer than the first: %v", *fx.pauses)
	}

	fx.fake.DbErrors = 1000
	asked := len(fx.fake.Requests())
	res = fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
	if !res.OK || res.Source != "git" || len(res.Notes) != 1 || res.Notes[0] != "github_api_skipped: proxy_db_error" || len(fx.fake.Requests())-asked != 3 {
		t.Fatalf("a proxy that stays down ends in a clone: %+v", res)
	}
	if fx.fetcher.GitHub.keyRejected() {
		t.Fatal("a passing fault is no reason to stay away from the proxy")
	}
}

// A key the proxy refuses is this deployment's mistake. The capture is cloned,
// and the proxy is left alone for a while instead of being asked once per ship.
func TestProxyKeyRefusedClonesAndCoolsOff(t *testing.T) {
	for _, code := range []string{"MISSING_API_KEY", "INVALID_API_KEY", "API_KEY_DISABLED"} {
		t.Run(code, func(t *testing.T) {
			fx := setupApi(t)
			if code == "INVALID_API_KEY" {
				fx.fetcher.GitHub.apiKey = "key2"
			} else {
				fx.fake.KeyError = code
			}
			clock := time.Now()
			fx.fetcher.GitHub.now = func() time.Time { return clock }

			res := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
			if !res.OK || res.Source != "git" || len(res.Commits) != 5 {
				t.Fatalf("a refused key must not cost the capture: %+v", res)
			}
			if len(res.Notes) != 1 || res.Notes[0] != "github_api_skipped: proxy_"+strings.ToLower(code) {
				t.Fatalf("notes: %q", res.Notes)
			}
			if len(fx.fake.Requests()) != 1 || len(*fx.pauses) != 0 {
				t.Fatalf("one refusal is enough, with no retry: %q", fx.fake.Requests())
			}

			again := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
			file := fx.fetcher.FetchFile(context.Background(), fx.repoUrl, "main.txt")
			if !again.OK || again.Source != "git" || !file.OK || len(fx.fake.Requests()) != 1 {
				t.Fatalf("during the cool-off the proxy is not called at all: %d requests", len(fx.fake.Requests()))
			}
			if len(again.Notes) != 1 || again.Notes[0] != "github_api_skipped: proxy_key_rejected" {
				t.Fatalf("notes: %q", again.Notes)
			}

			clock = clock.Add(11 * time.Minute)
			fx.fake.KeyError = ""
			fx.fetcher.GitHub.apiKey = fx.fake.Key
			fixed := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
			if !fixed.OK || fixed.Source != "github" {
				t.Fatalf("after the cool-off a corrected key is used again: %+v", fixed)
			}
		})
	}
}

// The proxy answering NOT_FOUND in its own envelope means this service asked for
// a path the proxy does not have: a bug here. GitHub's 404, forwarded, means the
// repository is private or gone. Only the second may reject a ship.
func TestProxyNotFoundIsNotARepositoryNotFound(t *testing.T) {
	for _, code := range []string{"NOT_FOUND", "METHOD_NOT_ALLOWED", "REQUEST_TOO_LARGE"} {
		fx := setupApi(t)
		fx.fake.ProxyError = code
		res := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
		if !res.OK || res.Source != "git" || len(res.Commits) != 5 {
			t.Fatalf("%s: the capture is cloned: %+v", code, res)
		}
		if len(res.Notes) != 1 || res.Notes[0] != "github_api_skipped: proxy_"+strings.ToLower(code) || len(fx.fake.Requests()) != 1 {
			t.Fatalf("%s: notes %q after %d requests", code, res.Notes, len(fx.fake.Requests()))
		}
	}

	// a 404 from something that is neither the proxy nor GitHub: a wrong url in
	// the configuration must not reject every ship either
	wrong := setupApi(t)
	wrong.fake.Status = http.StatusNotFound
	res := wrong.fetcher.FetchCommits(context.Background(), wrong.repoUrl, shipWindow)
	if !res.OK || res.Source != "git" || res.Notes[0] != "github_api_skipped: unexpected_404" {
		t.Fatalf("an unrecognisable 404 is cloned: %+v", res)
	}

	missing := setupApi(t)
	res = missing.fetcher.FetchCommits(context.Background(), strings.Replace(missing.repoUrl, "project1", "project404", 1), shipWindow)
	if res.OK || !res.Transient || errorCode(res.Error) != "clone_failed" || res.Source != "github" || len(res.Notes) != 0 {
		t.Fatalf("GitHub's own 404 is the inaccessible repository, with no fallback: %+v", res)
	}
}

// Exceeding the proxy's allowance is a one second matter: wait what it says and
// carry on through the api.
func TestProxyThrottleIsWaitedOut(t *testing.T) {
	fx := setupApi(t)
	fx.fake.Throttled = 3
	res := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
	if !res.OK || res.Source != "github" || len(res.Commits) != 5 || len(res.Notes) != 0 {
		t.Fatalf("a 429 with Retry-After is honored and the capture completes through the api: %+v", res)
	}
	waited := 0
	for _, pause := range *fx.pauses {
		if pause > 1250*time.Millisecond {
			t.Fatalf("Retry-After was one second, plus jitter: waited %s", pause)
		}
		if pause > 900*time.Millisecond {
			waited++
		}
	}
	if waited == 0 {
		t.Fatalf("the wait the proxy asked for must be taken: %v", *fx.pauses)
	}

	fx.fake.Throttled = 1000
	asked := len(fx.fake.Requests())
	res = fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
	if !res.OK || res.Source != "git" || res.Notes[0] != "github_api_skipped: proxy_rate_limited" || len(fx.fake.Requests())-asked != 4 {
		t.Fatalf("a proxy that keeps refusing ends in a clone after a few tries: %+v", res)
	}
	fx.fake.Throttled = 0
	if next := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow); next.Source != "github" {
		t.Fatalf("the next capture uses the api again: %+v", next)
	}
}

// The limiter alone, on a clock the test owns: however many callers ask at
// once, no window holds more sends than the quota.
func TestLimiterKeepsABurstInsideTheQuota(t *testing.T) {
	clock := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	l := &limiter{quota: 10, window: time.Second, now: func() time.Time { return clock }}
	var sends []time.Time
	l.sleep = func(_ context.Context, d time.Duration) error {
		clock = clock.Add(d)
		return nil
	}
	send := func(n int) {
		for range n {
			if err := l.wait(context.Background()); err != nil {
				t.Fatal(err)
			}
			sends = append(sends, clock)
		}
	}
	within := func(quota int, window time.Duration) {
		t.Helper()
		for i := range sends {
			if i+quota < len(sends) && sends[i+quota].Sub(sends[i]) < window {
				t.Fatalf("sends %d to %d are %s apart: more than %d in a window", i, i+quota, sends[i+quota].Sub(sends[i]), quota)
			}
		}
	}

	send(35)
	within(10, time.Second)
	if took := sends[34].Sub(sends[0]); took < 3*time.Second || took > 4*time.Second {
		t.Fatalf("35 sends at the default of 10 a second take a little over 3 seconds, took %s", took)
	}

	sends = nil
	l.observe(http.Header{"Ratelimit-Policy": {`"default";q=3;w=2`}, "Ratelimit-Remaining": {"2"}, "Ratelimit-Reset": {"2"}})
	if l.quota != 3 || l.window != 2*time.Second {
		t.Fatalf("the advertised policy sizes the limiter: %d per %s", l.quota, l.window)
	}
	send(10)
	within(3, 2*time.Second)

	l.observe(http.Header{"Ratelimit-Limit": {"50"}, "Ratelimit-Remaining": {"0"}, "Ratelimit-Reset": {"4"}})
	if l.quota != 50 {
		t.Fatalf("without a policy the limit header gives the quota: %d", l.quota)
	}
	before := clock
	send(1)
	if clock.Sub(before) < 4*time.Second {
		t.Fatalf("an allowance reported as spent is waited out: %s", clock.Sub(before))
	}
	l.observe(http.Header{"Ratelimit-Policy": {`"default";q=0;w=0`}})
	if l.quota != 50 || l.window != 2*time.Second {
		t.Fatal("a nonsense policy changes nothing")
	}
}

// End to end against a proxy that really enforces a small allowance: the
// capture needs more than twice the quota, and not one request is refused.
func TestGitHubCaptureStaysInsideTheProxyAllowance(t *testing.T) {
	fx := setupApi(t)
	fx.fake.Quota = 5
	fx.fetcher.GitHub.pageSize = 2
	real := NewGitHub("", "", "") // this one really waits
	fx.fetcher.GitHub.now, fx.fetcher.GitHub.sleep = real.now, real.sleep
	res := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
	if !res.OK || res.Source != "github" || len(res.Commits) != 5 {
		t.Fatalf("capture: %+v", res)
	}
	if len(fx.fake.Requests()) < 11 || fx.fake.Refused() != 0 {
		t.Fatalf("%d requests against a quota of 5 a second, %d refused", len(fx.fake.Requests()), fx.fake.Refused())
	}
	if fx.fetcher.GitHub.limiter.quota != 5 {
		t.Fatalf("the limiter takes its size from the proxy: %d", fx.fetcher.GitHub.limiter.quota)
	}
}

// GitHub's Link headers name api.github.com. Only their page numbers are used:
// the proxyOnly transport fails the test if any request goes anywhere else.
func TestGitHubNeverFollowsALinkOutOfTheProxy(t *testing.T) {
	fx := apiOnly(t, buildHistory(t))
	fx.fetcher.GitHub.pageSize = 1
	fx.fake.FilesPerPage = 1
	fx.fake.RenamedFrom = "owner1/oldname"
	res := fx.fetcher.FetchCommits(context.Background(), "http://127.0.0.1:9/owner1/oldname", Window{})
	if !res.OK || res.Source != "github" || len(res.Commits) != 7 || res.OutsideWindow != 0 {
		t.Fatalf("paged lists, paged files and a redirect, all through the proxy: %+v", res)
	}
	if got := fx.fetcher.FetchRepoContext(context.Background(), "http://127.0.0.1:9/owner1/oldname"); !got.OK || got.Readme != "# project1\n" {
		t.Fatalf("every read of a renamed repository is redirected inside the proxy: %+v", got)
	}
	for _, request := range fx.fake.Requests() {
		if !strings.HasPrefix(request, "/gh/") {
			t.Fatalf("request outside the proxy's GitHub prefix: %s", request)
		}
	}
}

// The proxy serves a repeated url from its cache for minutes. A capture right
// after a push must still see the push: the head is looked up with a url that
// is new every second, and everything else is addressed by that commit.
func TestGitHubSeesAPushDespiteTheProxyCache(t *testing.T) {
	src := buildHistory(t)
	fx := apiOnly(t, src)
	fx.fake.Cached = true
	clock := time.Now()
	fx.fetcher.GitHub.now = func() time.Time { return clock }
	window := Window{Since: at(2), Until: at(20)}

	first := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, window)
	if !first.OK || len(first.Commits) != 6 || len(first.Files) != 5 {
		t.Fatalf("first capture: %+v", first)
	}
	context1 := fx.fetcher.FetchRepoContext(context.Background(), fx.repoUrl)
	if !context1.OK || context1.Readme != "# project1\n" {
		t.Fatalf("first readme: %+v", context1)
	}

	writeFile(t, src, "README.md", "# project1\n\nnow with instructions\n")
	writeFile(t, src, "pushed.txt", "pushed\n")
	gitAt(t, src, day(14), "add", ".")
	gitAt(t, src, day(14), "commit", "-q", "-m", "pushed a moment ago")
	gitAt(t, src, day(14), "push", "-q", fx.bare, "trunk")
	clock = clock.Add(2 * time.Second)

	second := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, window)
	if !second.OK || len(second.Commits) != 7 || second.Commits[0].Message != "pushed a moment ago" {
		t.Fatalf("the same ship read again seconds after a push must list it: %q", subjects(second.Commits))
	}
	if len(second.Files) != 6 || second.OutsideWindow != 1 {
		t.Fatalf("the listing is the pushed commit's: %d files, %d outside", len(second.Files), second.OutsideWindow)
	}
	context2 := fx.fetcher.FetchRepoContext(context.Background(), fx.repoUrl)
	if context2.Readme != "# project1\n\nnow with instructions\n" {
		t.Fatalf("the README is read at the pushed commit: %q", context2.Readme)
	}
	if file := fx.fetcher.FetchFile(context.Background(), fx.repoUrl, "pushed.txt"); !file.OK || file.Content != "pushed\n" {
		t.Fatalf("a file pushed a moment ago is readable: %+v", file)
	}
}

func TestGitHubUnreachableProxyFallsBack(t *testing.T) {
	fx := setupApi(t)
	fx.fetcher.GitHub.proxyUrl = "http://127.0.0.1:9"
	res := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
	if !res.OK || res.Source != "git" || len(res.Notes) != 1 || res.Notes[0] != "github_api_skipped: unreachable" || len(*fx.pauses) != 2 {
		t.Fatalf("a network failure is retried, then falls back to a clone: %+v %v", res, *fx.pauses)
	}
}

// When neither the api nor the clone reaches the repository and the api's
// failure was not about the repository, nothing was learned about it.
func TestGitHubUnavailableWhenBothPathsFail(t *testing.T) {
	for name, breakIt := range map[string]func(*testgit.GitHubFake){
		"github rate limit": func(fake *testgit.GitHubFake) { fake.Limited = 1000 },
		"proxy throttle":    func(fake *testgit.GitHubFake) { fake.Throttled = 1000 },
		"proxy db error":    func(fake *testgit.GitHubFake) { fake.DbErrors = 1000 },
		"github 502":        func(fake *testgit.GitHubFake) { fake.Broken = 1000 },
	} {
		fx := apiOnly(t, buildHistory(t))
		breakIt(fx.fake)
		res := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
		if res.OK || !res.Transient || errorCode(res.Error) != "github_unavailable" {
			t.Fatalf("%s: both paths failing is github_unavailable, never clone_failed: %+v", name, res)
		}
	}

	bad := apiOnly(t, buildHistory(t))
	bad.fake.KeyError = "INVALID_API_KEY"
	res := bad.fetcher.FetchCommits(context.Background(), bad.repoUrl, shipWindow)
	if res.OK || !res.Transient || errorCode(res.Error) != "clone_failed" {
		t.Fatalf("a refused key is our problem: the clone's verdict stands, as without a proxy: %+v", res)
	}
	if len(res.Notes) != 1 || res.Notes[0] != "github_api_skipped: proxy_invalid_api_key" {
		t.Fatalf("notes: %q", res.Notes)
	}
}

func TestGitHubWithoutAProxyClones(t *testing.T) {
	fx := setupApi(t)
	for name, github := range map[string]*GitHub{
		"nothing set": NewGitHub("", "", fx.fetcher.GitHub.host),
		"no key":      NewGitHub(fx.fake.URL, "", fx.fetcher.GitHub.host),
		"no url":      NewGitHub("", fx.fake.Key, fx.fetcher.GitHub.host),
	} {
		github.client.Transport = proxyOnly{t: t, github: &GitHub{proxyUrl: "http://no-http-at-all.invalid"}}
		fx.fetcher.GitHub = github
		res := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl+"/tree/trunk/src", shipWindow)
		if !res.OK || res.Source != "git" || len(res.Commits) != 5 || len(res.Notes) != 0 {
			t.Fatalf("%s: the repository is cloned: %+v", name, res)
		}
		file := fx.fetcher.FetchFile(context.Background(), fx.repoUrl, "main.txt")
		readme := fx.fetcher.FetchRepoContext(context.Background(), fx.repoUrl)
		if !file.OK || !readme.OK || len(fx.fake.Requests()) != 0 {
			t.Fatalf("%s: no http at all without the proxy's url and key: %q", name, fx.fake.Requests())
		}
		if !strings.HasSuffix(res.Commits[0].Url, "/owner1/project1/commit/"+res.Commits[0].Hash) {
			t.Fatalf("a pasted /tree/ page still names the repository: %s", res.Commits[0].Url)
		}
	}
}

func TestGitHubTruncatedTreeIsUnread(t *testing.T) {
	fx := apiOnly(t, buildHistory(t))
	fx.fake.TruncatedTree = true
	res := fx.fetcher.FetchCommits(context.Background(), fx.repoUrl, shipWindow)
	if !res.OK || res.TreeRead || len(res.Files) != 0 || res.Readme != "" {
		t.Fatalf("a listing GitHub cut short is unknown, not a short repository: %+v", res)
	}
	if len(res.Commits) != 5 {
		t.Fatalf("commits are unaffected: %d", len(res.Commits))
	}

	repo := fx.fetcher.FetchRepoContext(context.Background(), fx.repoUrl)
	if !repo.OK || !repo.PathsTruncated || repo.ReadmeName != "README.md" || repo.Readme != "# project1\n" {
		t.Fatalf("the README is still found at the root of a cut listing: %+v", repo)
	}
}

func TestGitHubReadsFilesAndReadme(t *testing.T) {
	fx := apiOnly(t, buildHistory(t))
	got := fx.fetcher.FetchFile(context.Background(), fx.repoUrl, "main.txt")
	if !got.OK || got.Binary || got.Truncated || got.Content != "main\nwork\n" {
		t.Fatalf("file: %+v", got)
	}
	missing := fx.fetcher.FetchFile(context.Background(), fx.repoUrl, "not/there.txt")
	if missing.OK || errorCode(missing.Error) != "read_failed" {
		t.Fatalf("a path the branch does not have is read_failed: %+v", missing)
	}
	if bad := fx.fetcher.FetchFile(context.Background(), fx.repoUrl, "/etc/passwd"); bad.Error != "bad_path" || requestsMatching(fx.fake, "passwd") != 0 {
		t.Fatalf("an unsafe path never reaches the api: %+v", bad)
	}

	repo := fx.fetcher.FetchRepoContext(context.Background(), fx.repoUrl)
	if !repo.OK || repo.Error != "" || repo.PathsTruncated || repo.ReadmeName != "README.md" || repo.Readme != "# project1\n" {
		t.Fatalf("readme: %+v", repo)
	}
	if !equal(sorted(repo.Paths), []string{"README.md", "later.txt", "main.txt", "side.txt", "src/b.txt"}) {
		t.Fatalf("paths: %q", repo.Paths)
	}

	gone := fx.fetcher.FetchRepoContext(context.Background(), "http://127.0.0.1:9/owner1/project404")
	if gone.OK || errorCode(gone.Error) != "clone_failed" {
		t.Fatalf("a missing repository cannot be read: %+v", gone)
	}
}

// A proxy that hands back the JSON form of a file, whatever form was asked for,
// must not have its envelope shown to a reviewer as source.
func TestGitHubDecodesTheJsonFormOfAFile(t *testing.T) {
	fx := apiOnly(t, buildHistory(t))
	fx.fake.JsonContents = true
	got := fx.fetcher.FetchFile(context.Background(), fx.repoUrl, "src/b.txt")
	if !got.OK || got.Content != strings.Repeat("a line of project1\n", 20)+"one more\n" {
		t.Fatalf("file: %+v", got)
	}
	if repo := fx.fetcher.FetchRepoContext(context.Background(), fx.repoUrl); repo.Readme != "# project1\n" {
		t.Fatalf("readme: %+v", repo)
	}
}

func TestGitHubCutsLargeAndBinaryFiles(t *testing.T) {
	src := buildHistory(t)
	writeFile(t, src, "big.txt", strings.Repeat("0123456789abcdef\n", MaxPreviewBytes/17+10))
	writeFile(t, src, "image.bin", "\x89PNG\x00\x00binary")
	gitAt(t, src, day(14), "add", ".")
	gitAt(t, src, day(14), "commit", "-q", "-m", "assets")
	fx := apiOnly(t, src)

	big := fx.fetcher.FetchFile(context.Background(), fx.repoUrl, "big.txt")
	if !big.OK || !big.Truncated || len(big.Content) != MaxPreviewBytes {
		t.Fatalf("a large file is cut at the preview size: ok=%v truncated=%v bytes=%d", big.OK, big.Truncated, len(big.Content))
	}
	binary := fx.fetcher.FetchFile(context.Background(), fx.repoUrl, "image.bin")
	if !binary.OK || !binary.Binary || binary.Content != "" {
		t.Fatalf("a binary file has no source to show: %+v", binary)
	}
}

// subjectOf and coAuthorTrailers stand in for git's %s and %(trailers) when the
// message comes from the api, so they are checked against git itself.
func TestMessageParsingMatchesGit(t *testing.T) {
	messages := []string{
		"plain title",
		"title\n\nbody only",
		"title\n\nCo-authored-by: User Two <user2@example.com>",
		"title\n\nbody\n\nCo-authored-by: User Two <user2@example.com>\nCo-authored-by: User Three <user3@example.com>\n",
		"title\n\nCo-authored-by: Early <user5@example.com>\n\nlast paragraph is prose",
		"title\n\nprose line\nCo-authored-by: User Two <user2@example.com>",
		"title\n\nprose line\nSigned-off-by: User One <user1@example.com>\nCo-authored-by: User Two <user2@example.com>",
		"title\n\nReviewed-by: User Six <user6@example.com>\nco-authored-by:   User Two <user2@example.com>  \nCO-AUTHORED-BY: User Three <user3@example.com>",
		"Co-authored-by: Title Only <user7@example.com>",
		"line one\nline two\n\nbody",
		"title\n\nCo-authored-by: User Two <user2@example.com>\n\n\n",
		"title\n\nNot a trailer: because of spaces <user8@example.com>\nCo-authored-by: User Two <user2@example.com>",
		"title\n\nCo-authored-by : User Two <user2@example.com>",
	}
	dir := t.TempDir()
	gitAt(t, dir, day(1), "init", "-q", "-b", "main")
	for _, message := range messages {
		gitAt(t, dir, day(1), "commit", "-q", "--allow-empty", "--cleanup=verbatim", "-m", message)
		out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%s%x1f%(trailers:key=Co-authored-by,valueonly)%x1f%B").Output()
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.SplitN(string(out), "\x1f", 3)
		stored := strings.TrimSuffix(fields[2], "\n")
		if got := subjectOf(stored); got != fields[0] {
			t.Fatalf("subject of %q: got %q, git says %q", message, got, fields[0])
		}
		got, want := ParseCoAuthors(coAuthorTrailers(stored)), ParseCoAuthors(fields[1])
		if len(got) != len(want) {
			t.Fatalf("co-authors of %q: got %+v, git says %+v", message, got, want)
		}
		for i := range want {
			if deref(got[i].Name) != deref(want[i].Name) || deref(got[i].Email) != deref(want[i].Email) {
				t.Fatalf("co-author %d of %q: got %+v, git says %+v", i, message, got[i], want[i])
			}
		}
	}
}
