package githost

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hackclub/ari-webhooks/internal/testgit"
)

func gitRunOrFail(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Mia Maker", "GIT_AUTHOR_EMAIL=Mia@Example.com",
		"GIT_COMMITTER_NAME=Mia Maker", "GIT_COMMITTER_EMAIL=mia@example.com",
		"GIT_AUTHOR_DATE=2026-06-20T10:00:00Z", "GIT_COMMITTER_DATE=2026-06-20T10:00:00Z",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// buildRepo creates a real repo with two commits (one carrying a co-author
// trailer) and returns its path.
func buildRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRunOrFail(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunOrFail(t, dir, "add", ".")
	gitRunOrFail(t, dir, "commit", "-q", "-m", "first commit")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\nthree\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunOrFail(t, dir, "add", ".")
	gitRunOrFail(t, dir, "commit", "-q", "-m", "second commit\n\nbody text\n\nCo-authored-by: Pal Person <PAL@example.com>\nCo-authored-by: Claude <noreply@anthropic.com>")
	return dir
}

// TestParseLogAgainstRealGit runs the exact log command the fetcher uses over a
// real repo, so the parser is validated against genuine git output.
func TestParseLogAgainstRealGit(t *testing.T) {
	dir := buildRepo(t)
	cmd := exec.Command("git", logArgs(Window{}, 10)...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	commits := ParseLog(string(out), "https://github.com/mia/proj.git")
	if len(commits) != 2 {
		t.Fatalf("parsed %d commits", len(commits))
	}
	second, first := commits[0], commits[1] // log is newest first
	if first.Message != "first commit" || len(first.Paths) != 1 || first.Paths[0] != "a.txt" {
		t.Fatalf("first commit: %+v", first)
	}
	if second.Message != "second commit" || len(second.Paths) != 1 || second.Paths[0] != "a.txt" {
		t.Fatalf("second commit: %+v", second)
	}
	if first.LineStats || second.LineStats || second.Additions != 0 || second.Deletions != 0 {
		t.Fatalf("a log without file contents counts no lines: %+v", second)
	}
	if first.AuthorName != "Mia Maker" || first.AuthorEmail != "mia@example.com" {
		t.Fatalf("author identity (email lowercased): %q %q", first.AuthorName, first.AuthorEmail)
	}
	if len(second.CoAuthors) != 2 || *second.CoAuthors[0].Name != "Pal Person" || *second.CoAuthors[0].Email != "pal@example.com" {
		t.Fatalf("co-authors: %+v", second.CoAuthors)
	}
	if second.Url != "https://github.com/mia/proj/commit/"+second.Hash {
		t.Fatalf("commit url: %s", second.Url)
	}
	if !first.CommittedAt.Equal(time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("committedAt: %v", first.CommittedAt)
	}
}

func TestParseCoAuthorsShapes(t *testing.T) {
	got := ParseCoAuthors("A One <a@x.com>\n\nbare name\n<only@x.com>\nA One <A@X.COM>\n")
	if len(got) != 3 {
		t.Fatalf("co-authors: %+v", got)
	}
	if *got[0].Name != "A One" || *got[0].Email != "a@x.com" {
		t.Fatalf("row 0: %+v", got[0])
	}
	if *got[1].Name != "bare name" || got[1].Email != nil {
		t.Fatalf("bare name row: %+v", got[1])
	}
	if got[2].Name != nil || got[2].Email == nil || *got[2].Email != "only@x.com" {
		// A bare <email> line parses with an empty name, exactly like the TS regex.
		t.Fatalf("bare email row: %+v", got[2])
	}
}

// TestFetchCommitsOverSmartHttp exercises the full clone + log + parse path
// against a real git server.
func TestFetchCommitsOverSmartHttp(t *testing.T) {
	src := buildRepo(t)
	root := t.TempDir()
	bare := filepath.Join(root, "proj.git")
	gitRunOrFail(t, src, "clone", "-q", "--bare", src, bare)
	srv := testgit.Serve(t, root)

	f := NewFetcher(2, 0)
	f.AllowPrivateHosts = true // httptest listens on 127.0.0.1
	res := f.FetchCommits(context.Background(), srv.URL+"/proj.git", Window{})
	if !res.OK || len(res.Commits) != 2 {
		t.Fatalf("fetch over smart http: %+v", res)
	}
}

// TestFetchFileOverSmartHttp exercises the on-demand read path: blobless clone,
// then a cat-file that faults in the single blob from the promisor remote.
func TestFetchFileOverSmartHttp(t *testing.T) {
	src := buildRepo(t)
	root := t.TempDir()
	bare := filepath.Join(root, "proj.git")
	gitRunOrFail(t, src, "clone", "-q", "--bare", src, bare)
	srv := testgit.Serve(t, root)

	f := NewFetcher(2, 0)
	f.AllowPrivateHosts = true // httptest listens on 127.0.0.1
	got := f.FetchFile(context.Background(), srv.URL+"/proj.git", "a.txt")
	if !got.OK || got.Binary || got.Truncated || got.Content != "one\nthree\nfour\n" {
		t.Fatalf("fetch file over smart http: %+v", got)
	}

	missing := f.FetchFile(context.Background(), srv.URL+"/proj.git", "not/there.txt")
	if missing.OK || !strings.HasPrefix(missing.Error, "read_failed") {
		t.Fatalf("a path HEAD does not have must fail as read_failed: %+v", missing)
	}
}

func TestFetchFileRefusesUnsafeShapes(t *testing.T) {
	f := NewFetcher(2, 0)
	for _, p := range []string{"", "/etc/passwd", "--upload-pack=evil"} {
		if got := f.FetchFile(context.Background(), "https://github.com/a/b", p); got.OK || got.Error != "bad_path" {
			t.Fatalf("path %q: %+v", p, got)
		}
	}
	if got := f.FetchFile(context.Background(), "http://10.0.0.1/repo.git", "a.txt"); got.OK || got.Error != "unsafe_url" {
		t.Fatalf("private host must be refused: %+v", got)
	}
}

func TestFetchCommitsBlocksUnsafeUrl(t *testing.T) {
	f := NewFetcher(2, 0)
	res := f.FetchCommits(context.Background(), "http://10.0.0.1/repo.git", Window{})
	if res.OK || res.Error != "unsafe_url" || res.Transient {
		t.Fatalf("private repo url must be a permanent unsafe_url: %+v", res)
	}
}

func TestFetchCommitsTransientOnDeadHost(t *testing.T) {
	f := NewFetcher(2, 0)
	f.AllowPrivateHosts = true
	res := f.FetchCommits(context.Background(), "http://127.0.0.1:9/nope.git", Window{})
	if res.OK || !res.Transient {
		t.Fatalf("dead host must be transient: %+v", res)
	}
	if noteCode(res.Error) != "clone_failed" && noteCode(res.Error) != "clone_timeout" {
		t.Fatalf("error code: %s", res.Error)
	}
}

func noteCode(s string) string {
	for i := range s {
		if s[i] == ':' {
			return s[:i]
		}
	}
	return s
}

func TestRunGitCapsStderr(t *testing.T) {
	r := runGit(context.Background(),
		[]string{"-c", "alias.spew=!yes error-spam 1>&2", "spew"},
		t.TempDir(), 2*time.Second)
	if r.ok {
		t.Fatal("spewing alias should not report ok")
	}
	if len(r.stderr) > 64<<10 {
		t.Fatalf("stderr grew past the cap: %d bytes buffered", len(r.stderr))
	}
}

func buildBigFileRepo(t *testing.T, bigBytes int) (root, bare string) {
	t.Helper()
	src := t.TempDir()
	gitRunOrFail(t, src, "init", "-q", "-b", "main")
	line := []byte("some generated data line for the big file fixture 0123456789\n")
	big := make([]byte, 0, bigBytes+len(line))
	for len(big) < bigBytes {
		big = append(big, line...)
	}
	if err := os.WriteFile(filepath.Join(src, "big.txt"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunOrFail(t, src, "add", ".")
	gitRunOrFail(t, src, "commit", "-q", "-m", "ship it")
	root = t.TempDir()
	bare = filepath.Join(root, "proj.git")
	gitRunOrFail(t, src, "clone", "-q", "--bare", src, bare)
	return root, bare
}

// A server that does not offer filters sends file contents anyway: sizes are
// known, and lines still go uncounted so every clone reads the same.
func TestFetchCommitsCountsNoLinesAndKeepsKnownSizes(t *testing.T) {
	root, _ := buildBigFileRepo(t, 2<<20)
	srv := testgit.Serve(t, root)

	f := NewFetcher(2, 0)
	f.AllowPrivateHosts = true
	res := f.FetchCommits(context.Background(), srv.URL+"/proj.git", Window{})
	if !res.OK || len(res.Commits) != 1 || res.Source != "git" {
		t.Fatalf("fetch: %+v", res)
	}
	c := res.Commits[0]
	if c.LineStats || c.Additions != 0 || c.Deletions != 0 {
		t.Fatalf("a clone counts no lines, got +%d -%d known=%v", c.Additions, c.Deletions, c.LineStats)
	}
	sizes := map[string]int64{}
	for _, file := range res.Files {
		sizes[file.Path] = file.Bytes
	}
	if sizes["main.go"] != 29 || sizes["big.txt"] < 2<<20 {
		t.Fatalf("contents the server sent anyway give real sizes: %v", sizes)
	}
}

func TestFetchCommitsAgainstFilterCapableServer(t *testing.T) {
	root, bare := buildBigFileRepo(t, 1<<10)
	gitRunOrFail(t, bare, "config", "uploadpack.allowFilter", "true")
	gitRunOrFail(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	srv := testgit.Serve(t, root)

	f := NewFetcher(2, 0)
	f.AllowPrivateHosts = true
	res := f.FetchCommits(context.Background(), srv.URL+"/proj.git", Window{})
	if !res.OK || len(res.Commits) != 1 || !res.TreeRead {
		t.Fatalf("fetch against filter-capable server: %+v", res)
	}
	if c := res.Commits[0]; c.LineStats || len(c.Paths) != 2 {
		t.Fatalf("a blobless clone gives paths and no line counts: %+v", c)
	}
	if len(res.Files) != 2 {
		t.Fatalf("files: %+v", res.Files)
	}
	for _, file := range res.Files {
		if len(file.Blob) != 40 || file.Bytes != UnknownBytes {
			t.Fatalf("a blobless listing has blob ids and no sizes: %+v", file)
		}
	}
}

func TestFindReadmeAcceptsAnyExtensionAndCasing(t *testing.T) {
	for _, listing := range [][]string{
		{"main.go", "README.md"},
		{"readme"},
		{"Readme.txt", "src"},
		{"README.rst"},
	} {
		if FindReadme(listing) == "" {
			t.Fatalf("listing %v has a README", listing)
		}
	}
	for _, listing := range [][]string{
		{"main.go", "LICENSE"},
		{"readmes"},          // not a README, just a similar name
		{"readme_helper.go"}, // ditto: the separator has to be a dot
		{"docs"},             // a nested README is not at the root
		{},
	} {
		if got := FindReadme(listing); got != "" {
			t.Fatalf("listing %v has no root README, got %q", listing, got)
		}
	}
}

func TestFetchCommitsReportsRootReadme(t *testing.T) {
	root, _ := buildBigFileRepo(t, 1<<10)
	srv := testgit.Serve(t, root)
	f := NewFetcher(2, 0)
	f.AllowPrivateHosts = true

	res := f.FetchCommits(context.Background(), srv.URL+"/proj.git", Window{})
	if !res.OK || !res.TreeRead {
		t.Fatalf("a healthy capture must read the root listing: %+v", res)
	}
	if res.Readme != "" {
		t.Fatalf("this fixture repo has no README, got %q", res.Readme)
	}

	withReadme := t.TempDir()
	gitRunOrFail(t, withReadme, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(withReadme, "README.md"), []byte("# proj\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunOrFail(t, withReadme, "add", ".")
	gitRunOrFail(t, withReadme, "commit", "-q", "-m", "docs")
	readmeRoot := t.TempDir()
	gitRunOrFail(t, withReadme, "clone", "-q", "--bare", withReadme, filepath.Join(readmeRoot, "proj.git"))

	res = f.FetchCommits(context.Background(), testgit.Serve(t, readmeRoot).URL+"/proj.git", Window{})
	if !res.OK || !res.TreeRead || res.Readme != "README.md" {
		t.Fatalf("root README must be reported: readme=%q treeRead=%v", res.Readme, res.TreeRead)
	}
}

func TestFindReadmeIgnoresNestedOnes(t *testing.T) {
	if got := FindReadme([]string{"src/main.go", "docs/README.md", "packages/app/readme.txt"}); got != "" {
		t.Fatalf("only a top-level README counts, got %q", got)
	}
	if got := FindReadme([]string{"docs/README.md", "README.md"}); got != "README.md" {
		t.Fatalf("the root one must win, got %q", got)
	}
}

func TestParseTreeReadsBlobIdsAndSizes(t *testing.T) {
	stdout := strings.Join([]string{
		"100644 blob 155fe69e707521adee38c3d9dc6d635212981ceb     311\tsrc/main.go",
		"100644 blob c2bd13b3750edd1a69d050cb484c770cb8940254    1954\tmy folder/a b.ts",
		"160000 commit aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa       -\tvendored-submodule",
		"100644 blob dddddddddddddddddddddddddddddddddddddddd     BAD\tunfetched.bin",
		"garbage line with no tab",
	}, "\x00")

	files := ParseTree(stdout)
	if len(files) != 3 {
		t.Fatalf("want the 3 blobs, got %d: %+v", len(files), files)
	}
	if files[2].Path != "unfetched.bin" || files[2].Bytes != UnknownBytes || files[2].Blob != strings.Repeat("d", 40) {
		t.Fatalf("a blob the clone never fetched keeps its id and has no size: %+v", files[2])
	}
	if files[0].Path != "src/main.go" || files[0].Bytes != 311 ||
		files[0].Blob != "155fe69e707521adee38c3d9dc6d635212981ceb" {
		t.Fatalf("first entry: %+v", files[0])
	}
	if files[1].Path != "my folder/a b.ts" || files[1].Bytes != 1954 {
		t.Fatalf("-z must keep spaces in paths verbatim: %+v", files[1])
	}
}

// TestFetchRepoContextOverSmartHttp exercises the live listing + README read
// the AI checks and the review screen's README tab share.
func TestFetchRepoContextOverSmartHttp(t *testing.T) {
	src := buildRepo(t)
	if err := os.WriteFile(filepath.Join(src, "ReadMe.md"), []byte("# Proj\n\nphotos below\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "cad"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "cad", "case.stl"), []byte("solid"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunOrFail(t, src, "add", ".")
	gitRunOrFail(t, src, "commit", "-q", "-m", "docs")
	root := t.TempDir()
	gitRunOrFail(t, src, "clone", "-q", "--bare", src, filepath.Join(root, "proj.git"))
	srv := testgit.Serve(t, root)

	f := NewFetcher(2, 0)
	f.AllowPrivateHosts = true // httptest listens on 127.0.0.1
	got := f.FetchRepoContext(context.Background(), srv.URL+"/proj.git")
	if !got.OK || got.Error != "" {
		t.Fatalf("fetch repo context: %+v", got)
	}
	if got.ReadmeName != "ReadMe.md" || got.Readme != "# Proj\n\nphotos below\n" || got.ReadmeTruncated {
		t.Fatalf("root README must come back verbatim, any casing: %+v", got)
	}
	paths := map[string]bool{}
	for _, p := range got.Paths {
		paths[p] = true
	}
	if !paths["a.txt"] || !paths["cad/case.stl"] {
		t.Fatalf("listing must be recursive: %v", got.Paths)
	}
}

func TestFetchRepoContextWithoutReadme(t *testing.T) {
	src := buildRepo(t)
	root := t.TempDir()
	gitRunOrFail(t, src, "clone", "-q", "--bare", src, filepath.Join(root, "proj.git"))
	srv := testgit.Serve(t, root)

	f := NewFetcher(2, 0)
	f.AllowPrivateHosts = true
	got := f.FetchRepoContext(context.Background(), srv.URL+"/proj.git")
	if !got.OK || got.ReadmeName != "" || got.Readme != "" {
		t.Fatalf("a repo without a root README reports none, not an error: %+v", got)
	}
}

func TestFetchRepoContextBlocksUnsafeUrl(t *testing.T) {
	f := NewFetcher(2, 0)
	got := f.FetchRepoContext(context.Background(), "http://10.0.0.1/repo.git")
	if got.OK || got.Error != "unsafe_url" {
		t.Fatalf("private repo url must be refused: %+v", got)
	}
}
