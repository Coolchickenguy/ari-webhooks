package enrich_test

import (
	"context"
	"testing"
	"time"

	"github.com/hackclub/ari-webhooks/internal/integrations/hackatime"
	"github.com/hackclub/ari-webhooks/internal/jobs"
)

type fileHoursRow struct {
	path    string
	seconds float64
	bytes   *int64
	status  string
}

func readFileHours(t *testing.T, f enrichFixture) map[string]fileHoursRow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		select path, seconds, bytes, status from ariw."submissionFileHours" where "submissionId" = $1`, f.subId)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]fileHoursRow{}
	for rows.Next() {
		var r fileHoursRow
		if err := rows.Scan(&r.path, &r.seconds, &r.bytes, &r.status); err != nil {
			t.Fatal(err)
		}
		out[r.path] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// End to end: the capture's per-file heartbeat accounting lands as one row per
// resolved path, repo paths for files the tree explains and bare tails for the
// rest, and a later degraded capture leaves the snapshot alone.
func TestEnrichRecordsPerFileHours(t *testing.T) {
	repoUrl := serveRepoWith(t, map[string]string{"src/game.ts": filler("game")})
	f := setupEnrich(t, repoUrl, []string{"snake"})
	ctx := context.Background()

	fourDaysAgo := float64(time.Now().Add(-4 * 24 * time.Hour).Unix())
	beats := append(
		runOn("/home/mia/snake/src/game.ts", "", fourDaysAgo, 10), // 9 gaps x 120s in the repo
		// 4 gaps x 120s nowhere, plus the capped 120s session-break credit the
		// re-opening beat carries.
		runOn("/home/mia/elsewhere/secret.ts", "", fourDaysAgo+7200, 5)...,
	)
	f.pipeline.Hackatime = &hackatime.Client{BaseUrl: serveHackatime(t, beats), AdminKey: "test"}
	if _, err := f.pool.Exec(ctx,
		`update "Maker" set "hackatimeUserId" = 'ht-1' where email = 'mia@example.com'`); err != nil {
		t.Fatal(err)
	}

	if outcome := f.pipeline.Handle(ctx, jobs.Job{SubmissionId: f.subId}); !outcome.IsDone() {
		t.Fatalf("outcome: %+v", outcome)
	}

	got := readFileHours(t, f)
	inRepo, ok := got["src/game.ts"]
	if !ok || inRepo.status != "head" || inRepo.seconds != 1080 {
		t.Fatalf("in-repo file must resolve to its repo path with its heartbeat seconds: %+v", got)
	}
	if inRepo.bytes == nil || *inRepo.bytes != int64(len(filler("game"))) {
		t.Fatalf("head rows must carry the tree's byte size: %+v", inRepo)
	}
	outside, ok := got["mia/elsewhere/secret.ts"]
	if !ok || outside.status != "none" || outside.seconds != 600 {
		t.Fatalf("an unexplained file must keep only its tail: %+v", got)
	}
	if outside.bytes != nil {
		t.Fatalf("only head rows carry bytes: %+v", outside)
	}
	// The repo file no heartbeat touched still gets a row, with zero time, so the
	// browser shows the whole repository.
	untouched, ok := got["main.go"]
	if !ok || untouched.status != "head" || untouched.seconds != 0 {
		t.Fatalf("an untouched repo file must appear with zero seconds: %+v", got)
	}
	if untouched.bytes == nil || *untouched.bytes != int64(len("package main\n")) {
		t.Fatalf("untouched head rows still carry the tree's byte size: %+v", untouched)
	}
	if len(got) != 3 {
		t.Fatalf("rows: %+v", got)
	}

	// A degraded Hackatime capture must not replace the snapshot with nothing.
	dead := &hackatime.Client{BaseUrl: "http://127.0.0.1:9", AdminKey: "test"}
	f.pipeline.Hackatime = dead
	outcome := f.pipeline.Handle(ctx, jobs.Job{SubmissionId: f.subId})
	_ = outcome // degraded captures may retry; only the rows matter here
	if after := readFileHours(t, f); len(after) != 3 {
		t.Fatalf("a degraded capture must leave the previous rows alone: %+v", after)
	}
}
