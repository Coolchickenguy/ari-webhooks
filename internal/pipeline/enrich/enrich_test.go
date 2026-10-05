package enrich_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hackclub/ari-webhooks/internal/ids"
	"github.com/hackclub/ari-webhooks/internal/integrations/lapse"
	"github.com/hackclub/ari-webhooks/internal/jobs"
)

func TestEnrichCapturesCommitsAndPromotes(t *testing.T) {
	repoUrl := serveRepo(t)
	f := setupEnrich(t, repoUrl, nil)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `update "HoursBreakdown" set "programMinutes" = 60 where "submissionId" = $1`, f.subId); err != nil {
		t.Fatal(err)
	}

	outcome := f.pipeline.Handle(ctx, jobs.Job{SubmissionId: f.subId})
	if !outcome.IsDone() {
		t.Fatalf("healthy capture must settle the job: %+v", outcome)
	}

	var status string
	var evidenceSyncedAt *time.Time
	if err := f.pool.QueryRow(ctx,
		`select status::text, "evidenceSyncedAt" from "Submission" where id = $1`, f.subId).
		Scan(&status, &evidenceSyncedAt); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || evidenceSyncedAt == nil {
		t.Fatalf("capture must promote and stamp: %s syncedNil=%v", status, evidenceSyncedAt == nil)
	}

	var hash, message, authorEmail string
	var additions int
	if err := f.pool.QueryRow(ctx, `
		select hash, message, "authorEmail", additions from "Commit" where "submissionId" = $1`, f.subId).
		Scan(&hash, &message, &authorEmail, &additions); err != nil {
		t.Fatal(err)
	}
	if len(hash) != 7 || message != "ship it" || authorEmail != "mia@example.com" || additions != 0 {
		t.Fatalf("commit row (a clone counts no lines): %s %s %s %d", hash, message, authorEmail, additions)
	}
}

func TestEnrichRetriesTransientCloneThenRejectsOnExhaustion(t *testing.T) {
	f := setupEnrich(t, "http://127.0.0.1:9/dead.git", nil)
	f.pipeline.Git.AllowPrivateHosts = true
	ctx := context.Background()

	outcome := f.pipeline.Handle(ctx, jobs.Job{SubmissionId: f.subId, Attempt: 0})
	if !outcome.IsRetry() || outcome.NextAttempt() != 1 {
		t.Fatalf("transient clone failure must retry: %+v", outcome)
	}
	var commitRows int
	if err := f.pool.QueryRow(ctx, `select count(*) from "Commit" where "submissionId" = $1`, f.subId).Scan(&commitRows); err != nil {
		t.Fatal(err)
	}
	if commitRows != 0 {
		t.Fatal("a transient failure must not write an empty snapshot")
	}
	var status string
	if err := f.pool.QueryRow(ctx, `select status::text from "Submission" where id = $1`, f.subId).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "processing" {
		t.Fatalf("still processing during the retry window: %s", status)
	}

	outcome = f.pipeline.Handle(ctx, jobs.Job{SubmissionId: f.subId, Attempt: 3})
	if !outcome.IsDone() {
		t.Fatalf("exhausted window must settle: %+v", outcome)
	}
	if err := f.pool.QueryRow(ctx, `select status::text from "Submission" where id = $1`, f.subId).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" {
		t.Fatalf("clone_failed at exhaustion must auto-reject: %s", status)
	}
	var reason string
	if err := f.pool.QueryRow(ctx,
		`select meta->>'reason' from "ActivityEvent" where kind = 'REJECTED' and "submissionId" = $1`, f.subId).
		Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "inaccessible_repo" {
		t.Fatalf("reject reason: %s", reason)
	}
}

func TestEnrichUnsafeRepoRejectsImmediately(t *testing.T) {
	f := setupEnrich(t, "http://169.254.169.254/latest.git", nil)
	f.pipeline.Git.AllowPrivateHosts = false // exercise the real guard
	ctx := context.Background()
	outcome := f.pipeline.Handle(ctx, jobs.Job{SubmissionId: f.subId})
	if !outcome.IsDone() {
		t.Fatalf("unsafe url must settle immediately: %+v", outcome)
	}
	var status string
	if err := f.pool.QueryRow(ctx, `select status::text from "Submission" where id = $1`, f.subId).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" {
		t.Fatalf("unsafe repo must auto-reject without a retry window: %s", status)
	}
}

func TestEnrichWindowsOutPreWindowJournals(t *testing.T) {
	repoUrl := serveRepo(t)
	f := setupEnrich(t, repoUrl, nil)
	ctx := context.Background()

	trackStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := f.pool.Exec(ctx, `
		update "Program" set "trackingStartsAt" = $2
		where id = (select "programId" from "Submission" where id = $1)`,
		f.subId, trackStart); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		at      time.Time
		minutes int
	}{
		{time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC), 100}, // before the window: dropped
		{time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC), 40},
	} {
		if _, err := f.pool.Exec(ctx, `
			insert into "Devlog" (id, "submissionId", at, minutes, seconds, text, markdown)
			values ($1, $2, $3, $4, $4 * 60, 'j', 'j')`,
			ids.Cuid(), f.subId, entry.at, entry.minutes); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.pool.Exec(ctx,
		`update "HoursBreakdown" set "devlogMinutes" = 140 where "submissionId" = $1`, f.subId); err != nil {
		t.Fatal(err)
	}

	if outcome := f.pipeline.Handle(ctx, jobs.Job{SubmissionId: f.subId}); !outcome.IsDone() {
		t.Fatalf("outcome: %+v", outcome)
	}
	var devlogs, devlogMinutes int
	if err := f.pool.QueryRow(ctx, `select count(*) from "Devlog" where "submissionId" = $1`, f.subId).Scan(&devlogs); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `select "devlogMinutes" from "HoursBreakdown" where "submissionId" = $1`, f.subId).Scan(&devlogMinutes); err != nil {
		t.Fatal(err)
	}
	if devlogs != 1 || devlogMinutes != 40 {
		t.Fatalf("pre-window journal must drop and re-settle: %d rows, %d minutes", devlogs, devlogMinutes)
	}
}

// Lapse hands back a project's whole recording history with no window of its own, so
// an update ship of an already-approved project must credit only the clips recorded
// since that approval. Otherwise every timelapse the earlier ship was paid for is
// paid for a second time.
func TestEnrichWindowsLapseClipsToWindow(t *testing.T) {
	repoUrl := serveRepo(t)
	f := setupEnrich(t, repoUrl, []string{"proj"})
	ctx := context.Background()

	priorShippedAt := time.Now().Add(-20 * 24 * time.Hour)
	beforeApproval := priorShippedAt.Add(-5 * 24 * time.Hour)
	afterApproval := priorShippedAt.Add(5 * 24 * time.Hour)

	clips := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `[
			{"id": "c1", "name": "before", "duration": 3600, "createdAt": %q, "playbackUrl": "https://lapse.test/c1"},
			{"id": "c2", "name": "after", "duration": 1800, "createdAt": %q, "playbackUrl": "https://lapse.test/c2"}
		]`, beforeApproval.UTC().Format(time.RFC3339), afterApproval.UTC().Format(time.RFC3339))
	}))
	defer clips.Close()
	f.pipeline.Lapse = &lapse.Client{BaseUrl: clips.URL, ApiKey: "test"}

	if _, err := f.pool.Exec(ctx,
		`update "Maker" set "hackatimeUserId" = 'ht-1' where email = 'mia@example.com'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `update "Submission" set version = 2 where id = $1`, f.subId); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		insert into "Submission" (id, "programId", "externalId", version, "makerId", title, "repoUrl", "claimedHours", status, "receivedAt")
		select $1, "programId", "externalId", 1, "makerId", 'T', $2, 0, 'approved', $3
		from "Submission" where id = $4`,
		ids.ShipId(), repoUrl, priorShippedAt, f.subId); err != nil {
		t.Fatal(err)
	}

	if outcome := f.pipeline.Handle(ctx, jobs.Job{SubmissionId: f.subId}); !outcome.IsDone() {
		t.Fatalf("outcome: %+v", outcome)
	}

	var lapseMinutes int
	if err := f.pool.QueryRow(ctx,
		`select "lapseMinutes" from "HoursBreakdown" where "submissionId" = $1`, f.subId).Scan(&lapseMinutes); err != nil {
		t.Fatal(err)
	}
	if lapseMinutes != 30 {
		t.Fatalf("lapseMinutes = %d, want only the 30 minutes recorded after the prior approval", lapseMinutes)
	}
	rows, err := f.pool.Query(ctx,
		`select note from "ElapsedClip" where "submissionId" = $1 order by at`, f.subId)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var notes []string
	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			t.Fatal(err)
		}
		notes = append(notes, note)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0] != "after" {
		t.Fatalf("clip rows = %v, want only the one recorded after the prior approval", notes)
	}
}

// The outbound justification reports the dates the hours were counted over, so the
// window start the capture filtered against has to survive on the row.
func TestEnrichPersistsTrackingWindowStart(t *testing.T) {
	repoUrl := serveRepo(t)
	f := setupEnrich(t, repoUrl, nil)
	ctx := context.Background()

	trackStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := f.pool.Exec(ctx, `
		update "Program" set "trackingStartsAt" = $2
		where id = (select "programId" from "Submission" where id = $1)`,
		f.subId, trackStart); err != nil {
		t.Fatal(err)
	}

	if outcome := f.pipeline.Handle(ctx, jobs.Job{SubmissionId: f.subId}); !outcome.IsDone() {
		t.Fatalf("outcome: %+v", outcome)
	}
	var got *time.Time
	if err := f.pool.QueryRow(ctx,
		`select "trackingFromAt" from "HoursBreakdown" where "submissionId" = $1`, f.subId).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("trackingFromAt must be written so the justification can report a real range")
	}
	if !got.UTC().Equal(trackStart) {
		t.Fatalf("trackingFromAt = %s, want the program's tracking start %s", got.UTC(), trackStart)
	}
}

// Nothing floors the window when the program has no tracking start, the project has
// no approved prior ship, and the payload carried no _prior_credited_at. There is no
// date to report, and the epoch is not one: the justification omits the range on a
// null and would otherwise tell the program the hours were counted from 1/1/1970.
func TestEnrichLeavesTrackingWindowStartNullWhenUnbounded(t *testing.T) {
	repoUrl := serveRepo(t)
	f := setupEnrich(t, repoUrl, nil)
	ctx := context.Background()

	if outcome := f.pipeline.Handle(ctx, jobs.Job{SubmissionId: f.subId}); !outcome.IsDone() {
		t.Fatalf("outcome: %+v", outcome)
	}
	var got *time.Time
	if err := f.pool.QueryRow(ctx,
		`select "trackingFromAt" from "HoursBreakdown" where "submissionId" = $1`, f.subId).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("trackingFromAt = %s, want null for an unbounded window", got.UTC())
	}
}
