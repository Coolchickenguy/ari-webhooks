package outbound

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hackclub/ari-webhooks/internal/ids"
)

type justificationSeed struct {
	hackatimeProjects []string
	hackatimeUserId   *string
	trackingFromAt    *time.Time
	lapseUrls         []string
	technicalFeatures string
	deflationReason   string
	withReview        bool
	programOptedOut   bool
}

func seedJustification(t *testing.T, pool *pgxpool.Pool, programId string, seed justificationSeed) string {
	t.Helper()
	ctx := context.Background()
	// The block is a per-program opt-in. Set the column explicitly both ways: its
	// default lives in ari's migrations and has already flipped once (false to true
	// in 20260826010000_hours_justification_default_on), so a case that relied on
	// the default would silently test the wrong flow.
	if _, err := pool.Exec(ctx,
		`update "Program" set "hoursJustification" = $2 where id = $1`,
		programId, !seed.programOptedOut); err != nil {
		t.Fatal(err)
	}
	makerId, subId := ids.Cuid(), ids.Cuid()
	if _, err := pool.Exec(ctx,
		`insert into "Maker" (id, email, name, "hackatimeUserId") values ($1, $2, 'Mia', $3)`,
		makerId, subId+"@x.com", seed.hackatimeUserId); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into "Submission" (id, "programId", "externalId", "makerId", title, "repoUrl",
		                          "claimedHours", status, "hackatimeProjects", "receivedAt")
		values ($1, $2, 'ext', $3, 'T', 'https://github.com/a/b', 0, 'pending', $4, '2026-07-22T00:00:00Z')`,
		subId, programId, makerId, seed.hackatimeProjects); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`insert into "HoursBreakdown" ("submissionId", "trackingFromAt") values ($1, $2)`,
		subId, seed.trackingFromAt); err != nil {
		t.Fatal(err)
	}
	for i, url := range seed.lapseUrls {
		if _, err := pool.Exec(ctx, `
			insert into "ElapsedClip" (id, "submissionId", at, "lengthSeconds", note, url)
			values ($1, $2, $3, 60, '', $4)`,
			ids.Cuid(), subId, time.Date(2026, 7, 21, i, 0, 0, 0, time.UTC), url); err != nil {
			t.Fatal(err)
		}
	}
	if seed.withReview {
		reviewerId := ids.Cuid()
		if _, err := pool.Exec(ctx,
			`insert into "User" (id, email, name, "avatarColor") values ($1, $2, 'Rev', '#000')`,
			reviewerId, reviewerId+"@example.com"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			insert into "Review" (id, "submissionId", "reviewerId", decision, "noteToMaker", "auditNote",
			                      "fieldValues", checklist, "technicalFeatures", "deflationReason")
			values ($1, $2, $3, 'approved', 'n', 'a', '{}', '[]', $4, $5)`,
			ids.Cuid(), subId, reviewerId, seed.technicalFeatures, seed.deflationReason); err != nil {
			t.Fatal(err)
		}
	}
	return subId
}

func TestLoadJustificationFormatsEveryPart(t *testing.T) {
	f := setupOutbox(t, "https://example.com/hook")
	from := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	hackatimeId := "4821"
	subId := seedJustification(t, f.pool, f.programId, justificationSeed{
		hackatimeProjects: []string{"my-game", "my-game-server"},
		hackatimeUserId:   &hackatimeId,
		trackingFromAt:    &from,
		lapseUrls:         []string{"https://lapse.example.com/a", "https://lapse.example.com/b"},
		technicalFeatures: "realtime multiplayer, procedurally-generated worlds",
		deflationReason:   "Deflated from 10 to 6 hours: only 3 commits with code changes",
		withReview:        true,
	})

	got, err := f.worker.loadJustification(context.Background(), subId)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected a justification block")
	}
	// The handbook's format: "<project> M/D/YYYY-M/D/YYYY", comma-separated.
	want := "my-game 7/20/2026-7/22/2026, my-game-server 7/20/2026-7/22/2026"
	if got.HackatimeProjects != want {
		t.Errorf("hackatime_projects = %q, want %q", got.HackatimeProjects, want)
	}
	if got.HackatimeUserId != "4821" {
		t.Errorf("hackatime_user_id = %q, want %q", got.HackatimeUserId, "4821")
	}
	if got.LapseLinks != "https://lapse.example.com/a, https://lapse.example.com/b" {
		t.Errorf("lapse_links = %q", got.LapseLinks)
	}
}

// A ship parked in second pass already carries a reviewer's written justification, and
// ari sent it with that decision. The events from here are system events, so a fraud
// relay reports the evidence and none of the reviewer's words.
func TestLoadJustificationLeavesReviewerProseToAri(t *testing.T) {
	f := setupOutbox(t, "https://example.com/hook")
	from := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	subId := seedJustification(t, f.pool, f.programId, justificationSeed{
		hackatimeProjects: []string{"my-game"},
		trackingFromAt:    &from,
		technicalFeatures: "realtime multiplayer, procedurally-generated worlds",
		deflationReason:   "Deflated from 10 to 6 hours: only 3 commits with code changes",
		withReview:        true,
	})
	if _, err := f.pool.Exec(context.Background(),
		`update "Submission" set status = 'secondpass' where id = $1`, subId); err != nil {
		t.Fatal(err)
	}

	got, err := f.worker.loadJustification(context.Background(), subId)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected a justification block")
	}
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"technical_features", "deflation_reason"} {
		if bytes.Contains(wire, []byte(key)) {
			t.Errorf("payload carries %s, which belongs to the decision ari sent: %s", key, wire)
		}
	}
	if got.HackatimeProjects != "my-game 7/20/2026-7/22/2026" {
		t.Errorf("hackatime_projects = %q, want the evidence to still be reported", got.HackatimeProjects)
	}
}

// No Hackatime on the ship means no Hackatime id either, even though the maker has
// one on file - the id is only there to locate the heartbeats behind the projects.
func TestLoadJustificationOmitsHackatimeWhenUntracked(t *testing.T) {
	f := setupOutbox(t, "https://example.com/hook")
	from := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	hackatimeId := "4821"
	subId := seedJustification(t, f.pool, f.programId, justificationSeed{
		hackatimeUserId: &hackatimeId,
		trackingFromAt:  &from,
		lapseUrls:       []string{"https://lapse.example.com/a"}, // the ship's only evidence, so the block survives
	})

	got, err := f.worker.loadJustification(context.Background(), subId)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected a justification block")
	}
	if got.HackatimeProjects != "" {
		t.Errorf("hackatime_projects = %q, want empty", got.HackatimeProjects)
	}
	if got.HackatimeUserId != "" {
		t.Errorf("hackatime_user_id = %q, want empty without tracked projects", got.HackatimeUserId)
	}
	if got.LapseLinks != "https://lapse.example.com/a" {
		t.Errorf("lapse_links = %q", got.LapseLinks)
	}
}

// A ship captured before trackingFromAt existed reports bare project names rather
// than a date range that was never analyzed.
func TestLoadJustificationOmitsRangeWhenWindowUnknown(t *testing.T) {
	f := setupOutbox(t, "https://example.com/hook")
	subId := seedJustification(t, f.pool, f.programId, justificationSeed{
		hackatimeProjects: []string{"my-game"},
	})

	got, err := f.worker.loadJustification(context.Background(), subId)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected a justification block")
	}
	if got.HackatimeProjects != "my-game" {
		t.Errorf("hackatime_projects = %q, want bare name", got.HackatimeProjects)
	}
}

// A system auto-rejection has no captured evidence: the block is omitted from the
// payload entirely rather than sent as a bag of empty strings. A reviewer's written
// justification is not evidence and cannot hold the block open on its own.
func TestLoadJustificationNilWhenNothingToReport(t *testing.T) {
	f := setupOutbox(t, "https://example.com/hook")
	subId := seedJustification(t, f.pool, f.programId, justificationSeed{
		technicalFeatures: "custom PCB, USB-C power delivery",
		deflationReason:   "Deflated from 10 to 6 hours: only 3 commits with code changes",
		withReview:        true,
	})

	got, err := f.worker.loadJustification(context.Background(), subId)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected no block, got %+v", got)
	}
}

// A program on the legacy flow gets no justification, however much evidence the ship
// has: its reviewers were never asked for one, so a payload carrying half a block
// would read as an evidence record nobody signed off.
func TestLoadJustificationSkippedWhenOptedOut(t *testing.T) {
	f := setupOutbox(t, "https://example.com/hook")
	from := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	hackatimeId := "4821"
	subId := seedJustification(t, f.pool, f.programId, justificationSeed{
		hackatimeProjects: []string{"my-game"},
		hackatimeUserId:   &hackatimeId,
		trackingFromAt:    &from,
		lapseUrls:         []string{"https://lapse.example.com/a"},
		programOptedOut:   true,
	})

	got, err := f.worker.loadJustification(context.Background(), subId)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected no block for a program on the legacy flow, got %+v", got)
	}
}

func TestLoadJustificationUnknownSubmission(t *testing.T) {
	f := setupOutbox(t, "https://example.com/hook")
	got, err := f.worker.loadJustification(context.Background(), "nope")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected no block for an unknown submission, got %+v", got)
	}
}
