package enrich_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hackclub/ari-webhooks/internal/integrations/githost"
	"github.com/hackclub/ari-webhooks/internal/integrations/hackatime"
	"github.com/hackclub/ari-webhooks/internal/jobs"
	"github.com/hackclub/ari-webhooks/internal/pipeline/enrich/enrichtest"
	"github.com/hackclub/ari-webhooks/internal/testgit"
)

type commitRow struct {
	id, hash             string
	additions, deletions int
}

func readCommits(t *testing.T, f enrichFixture) []commitRow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		select id, hash, additions, deletions from "Commit" where "submissionId" = $1 order by "committedAt", hash`, f.subId)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []commitRow
	for rows.Next() {
		var row commitRow
		if err := rows.Scan(&row.id, &row.hash, &row.additions, &row.deletions); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func hasNote(notes []string, want string) bool {
	for _, note := range notes {
		if note == want {
			return true
		}
	}
	return false
}

// A GitHub repository is captured through the api, with line counts. When a
// later capture has to clone instead, the counts already stored are kept rather
// than overwritten with zeros.
func TestEnrichThroughGitHubKeepsLineCountsWhenALaterCaptureClones(t *testing.T) {
	repoUrl, fake, github := enrichtest.ServeGitHubRepo(t, nil)
	f := setupEnrich(t, repoUrl, nil)
	f.pipeline.Git.GitHub = github
	ctx := context.Background()

	if outcome := f.pipeline.Handle(ctx, jobs.Job{SubmissionId: f.subId}); !outcome.IsDone() {
		t.Fatalf("outcome: %+v", outcome)
	}
	first := readCommits(t, f)
	if len(first) != 1 || first[0].additions != 1 || first[0].deletions != 0 {
		t.Fatalf("the api capture stores line counts: %+v", first)
	}
	if len(fake.Requests()) == 0 {
		t.Fatal("the capture must have gone through the api")
	}

	fake.Limited = 1000
	result, err := f.pipeline.Recapture(ctx, f.subId)
	if err != nil || !result.OK {
		t.Fatalf("a rate limited recapture still completes through a clone: %+v %v", result, err)
	}
	if !hasNote(result.Notes, "github_api_skipped: rate_limited") {
		t.Fatalf("the fallback is noted: %q", result.Notes)
	}
	second := readCommits(t, f)
	if len(second) != 1 || second[0] != first[0] {
		t.Fatalf("same commit, same id, line counts kept:\n before %+v\n after  %+v", first, second)
	}
}

// A clone without file contents knows no line counts and no file sizes: zeros
// and NULL are stored, never a size or count read from thin air.
func TestEnrichFromABloblessCloneStoresNoSizes(t *testing.T) {
	repoUrl, _, _ := enrichtest.ServeGitHubRepo(t, map[string]string{"src/game.ts": filler("game")})
	f := setupEnrich(t, repoUrl, []string{"snake"})
	ctx := context.Background()

	fourDaysAgo := float64(time.Now().Add(-4 * 24 * time.Hour).Unix())
	f.pipeline.Hackatime = &hackatime.Client{
		BaseUrl:  serveHackatime(t, runOn("/home/user1/snake/src/game.ts", "", fourDaysAgo, 10)),
		AdminKey: "test",
	}
	if _, err := f.pool.Exec(ctx,
		`update "Maker" set "hackatimeUserId" = 'ht-1' where email = 'mia@example.com'`); err != nil {
		t.Fatal(err)
	}
	if outcome := f.pipeline.Handle(ctx, jobs.Job{SubmissionId: f.subId}); !outcome.IsDone() {
		t.Fatalf("outcome: %+v", outcome)
	}

	commits := readCommits(t, f)
	if len(commits) != 1 || commits[0].additions != 0 || commits[0].deletions != 0 {
		t.Fatalf("commits: %+v", commits)
	}
	got := readFileHours(t, f)
	if row, ok := got["src/game.ts"]; !ok || row.status != "head" || row.seconds != 1080 || row.bytes != nil {
		t.Fatalf("the file resolves and carries its time, with no size: %+v", got)
	}
	if row, ok := got["main.go"]; !ok || row.status != "head" || row.bytes != nil {
		t.Fatalf("untouched files are listed with no size: %+v", got)
	}
}

// GitHub being down or throttling says nothing about the repository: the ship
// keeps retrying on the slow loop and is never rejected as inaccessible.
func TestEnrichGitHubOutageNeverStartsTheRejectCountdown(t *testing.T) {
	_, fake, _ := enrichtest.ServeGitHubRepo(t, nil)
	f := setupEnrich(t, "http://127.0.0.1:9/owner1/project1", nil) // no git server there: the clone fails too
	f.pipeline.Git.GitHub = githost.NewGitHub(fake.URL, fake.Key, "127.0.0.1:9")
	fake.Limited = 1000
	ctx := context.Background()

	outcome := f.pipeline.Handle(ctx, jobs.Job{SubmissionId: f.subId, Attempt: 3})
	if !outcome.IsRetry() || outcome.NextAttempt() != 3 {
		t.Fatalf("an exhausted window must still retry, without spending an attempt: %+v", outcome)
	}
	_, commits, status := f.snapshot(t)
	if status != "processing" || commits != 0 {
		t.Fatalf("nothing is decided or written: %s %d", status, commits)
	}
}

// GitHub answering 404 is the repository's own answer: private, deleted or
// mistyped. Same outcome as a clone that fails.
func TestEnrichGitHubMissingRepositoryIsRejectedAsInaccessible(t *testing.T) {
	_, fake, _ := enrichtest.ServeGitHubRepo(t, nil)
	f := setupEnrich(t, "http://127.0.0.1:9/owner1/project404", nil)
	f.pipeline.Git.GitHub = githost.NewGitHub(fake.URL, fake.Key, "127.0.0.1:9")
	ctx := context.Background()

	outcome := f.pipeline.Handle(ctx, jobs.Job{SubmissionId: f.subId, Attempt: 0})
	if !outcome.IsRetry() || outcome.NextAttempt() != 1 {
		t.Fatalf("a missing repository is retried through the normal window: %+v", outcome)
	}
	outcome = f.pipeline.Handle(ctx, jobs.Job{SubmissionId: f.subId, Attempt: 3})
	if !outcome.IsDone() {
		t.Fatalf("exhausted window must settle: %+v", outcome)
	}
	var status, reason string
	if err := f.pool.QueryRow(ctx, `
		select s.status::text, (select meta->>'reason' from "ActivityEvent" where kind = 'REJECTED' and "submissionId" = s.id)
		from "Submission" s where s.id = $1`, f.subId).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" || reason != "inaccessible_repo" {
		t.Fatalf("status %s reason %s", status, reason)
	}
}

// Three commits a day apart, the ship's window holding only the middle one. The
// window is applied where the commits are read, and both ways of reading store
// the same commit.
func TestEnrichStoresTheSameCommitsWithTheWindowAtTheSource(t *testing.T) {
	src := t.TempDir()
	testgit.Run(t, src, "init", "-q", "-b", "main")
	for i, when := range []string{"2026-06-01T10:00:00Z", "2026-06-02T10:00:00Z", "2026-06-03T10:00:00Z"} {
		name := []string{"one.txt", "two.txt", "three.txt"}[i]
		if err := os.WriteFile(filepath.Join(src, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		testgit.RunAt(t, src, when, "add", ".")
		testgit.RunAt(t, src, when, "commit", "-q", "-m", "add "+name)
	}
	root := t.TempDir()
	bare := filepath.Join(root, "owner1", "project1")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	testgit.Run(t, src, "clone", "-q", "--bare", src, bare)
	srv := testgit.Serve(t, root)
	fake := testgit.ServeGitHub(t, bare, "owner1/project1")
	repoUrl := srv.URL + "/owner1/project1"

	stored := map[string][]string{}
	for _, source := range []string{"git", "github"} {
		t.Run(source, func(t *testing.T) {
			f := setupEnrich(t, repoUrl, nil)
			if source == "github" {
				f.pipeline.Git.GitHub = githost.NewGitHub(fake.URL, fake.Key, strings.TrimPrefix(srv.URL, "http://"))
			}
			ctx := context.Background()
			if _, err := f.pool.Exec(ctx, `
				update "Program" set "trackingStartsAt" = '2026-06-02T10:00:00Z'
				where id = (select "programId" from "Submission" where id = $1)`, f.subId); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(ctx,
				`update "Submission" set "receivedAt" = '2026-06-02T10:00:00.250Z' where id = $1`, f.subId); err != nil {
				t.Fatal(err)
			}
			result, err := f.pipeline.Enrich(ctx, f.subId)
			if err != nil || !result.OK || result.Commits != 1 {
				t.Fatalf("capture: %+v %v", result, err)
			}
			if !hasNote(result.Notes, "commits_outside_tracking_window_2") {
				t.Fatalf("the commits the window left out are still counted: %q", result.Notes)
			}
			rows, err := f.pool.Query(ctx, `select hash || ' ' || message from "Commit" where "submissionId" = $1 order by "committedAt"`, f.subId)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					t.Fatal(err)
				}
				stored[source] = append(stored[source], line)
			}
		})
	}
	if len(stored["git"]) != 1 || !strings.HasSuffix(stored["git"][0], " add two.txt") {
		t.Fatalf("only the commit on the window's edge is inside it: %q", stored["git"])
	}
	if strings.Join(stored["git"], "\n") != strings.Join(stored["github"], "\n") {
		t.Fatalf("both sources must store the same commits:\n git    %q\n github %q", stored["git"], stored["github"])
	}
}
