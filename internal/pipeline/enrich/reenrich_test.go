package enrich_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hackclub/ari-webhooks/internal/jobs"
	"github.com/hackclub/ari-webhooks/internal/testgit"
)

func TestReenrichRefreshesCommitsAndBumpsVersion(t *testing.T) {
	// Built by hand instead of serveRepo so a second commit can land in the
	// source repo between the two captures.
	src := t.TempDir()
	testgit.Run(t, src, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testgit.Run(t, src, "add", ".")
	testgit.Run(t, src, "commit", "-q", "-m", "ship it")
	root := t.TempDir()
	bare := filepath.Join(root, "proj.git")
	testgit.Run(t, src, "clone", "-q", "--bare", src, bare)
	repoUrl := testgit.Serve(t, root).URL + "/proj.git"

	f := setupEnrich(t, repoUrl, nil)
	ctx := context.Background()
	// The capture window closes at receivedAt; push it forward so the fix
	// committed below still falls inside it.
	if _, err := f.pool.Exec(ctx,
		`update "Submission" set "receivedAt" = now() + interval '1 hour' where id = $1`, f.subId); err != nil {
		t.Fatal(err)
	}

	if outcome := f.pipeline.Handle(ctx, jobs.Job{SubmissionId: f.subId}); !outcome.IsDone() {
		t.Fatalf("ingest capture must settle the job: %+v", outcome)
	}
	version, commits, status := f.snapshot(t)
	if version != 1 || commits != 1 || status != "pending" {
		t.Fatalf("after ingest capture: version=%d commits=%d status=%s", version, commits, status)
	}
	var originalCommitId string
	if err := f.pool.QueryRow(ctx,
		`select id from "Commit" where "submissionId" = $1`, f.subId).Scan(&originalCommitId); err != nil {
		t.Fatal(err)
	}

	// The maker fixes things up while the ship sits in the queue.
	if err := os.WriteFile(filepath.Join(src, "fix.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testgit.Run(t, src, "add", ".")
	testgit.Run(t, src, "commit", "-q", "-m", "fix it")
	testgit.Run(t, src, "push", "-q", bare, "main:main")

	if outcome := f.pipeline.HandleReenrich(ctx, jobs.Job{SubmissionId: f.subId}); !outcome.IsDone() {
		t.Fatalf("recapture must settle the job: %+v", outcome)
	}
	version, commits, status = f.snapshot(t)
	if version != 2 || commits != 2 || status != "pending" {
		t.Fatalf("after recapture: version=%d commits=%d status=%s", version, commits, status)
	}
	var preserved int
	if err := f.pool.QueryRow(ctx, `
		select count(*) from "Commit" where "submissionId" = $1 and id = $2`,
		f.subId, originalCommitId).Scan(&preserved); err != nil {
		t.Fatal(err)
	}
	if preserved != 1 {
		t.Fatal("recapture replaced an unchanged commit id, invalidating held per-item adjustments")
	}
}

func TestReenrichLeavesDecidedShipsAlone(t *testing.T) {
	f := setupEnrich(t, serveRepo(t), nil)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx,
		`update "Submission" set status = 'approved' where id = $1`, f.subId); err != nil {
		t.Fatal(err)
	}

	if outcome := f.pipeline.HandleReenrich(ctx, jobs.Job{SubmissionId: f.subId}); !outcome.IsDone() {
		t.Fatalf("a decided ship must settle the job untouched: %+v", outcome)
	}
	version, commits, status := f.snapshot(t)
	if version != 0 || commits != 0 || status != "approved" {
		t.Fatalf("decided snapshot must stay final: version=%d commits=%d status=%s", version, commits, status)
	}
}

// A second-pass confirm re-settles from the evidence, so the verifying organizer
// can refresh a held ship's stale snapshot; the hold itself must survive it.
func TestReenrichRefreshesAHeldSecondPassShip(t *testing.T) {
	f := setupEnrich(t, serveRepo(t), nil)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx,
		`update "Submission" set status = 'secondpass' where id = $1`, f.subId); err != nil {
		t.Fatal(err)
	}

	if outcome := f.pipeline.HandleReenrich(ctx, jobs.Job{SubmissionId: f.subId}); !outcome.IsDone() {
		t.Fatalf("recapture must settle the job: %+v", outcome)
	}
	version, commits, status := f.snapshot(t)
	if version != 1 || commits != 1 {
		t.Fatalf("the snapshot must refresh: version=%d commits=%d", version, commits)
	}
	if status != "secondpass" {
		t.Fatalf("the second-pass hold must survive a recapture: status=%s", status)
	}
}
