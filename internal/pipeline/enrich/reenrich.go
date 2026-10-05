package enrich

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hackclub/ari-webhooks/internal/jobs"
)

// HandleReenrich is the job handler for kind "reenrich": a reviewer-triggered
// re-capture of a ship already sitting in the review queue. Unlike the ingest
// capture there is a complete prior snapshot to fall back on, so a capture that
// stays degraded through the retry window just settles with the old snapshot
// still current - nothing is auto-rejected or promoted from here.
func (p *Pipeline) HandleReenrich(ctx context.Context, j jobs.Job) jobs.Outcome {
	var status string
	err := p.Pool.QueryRow(ctx, `select status::text from "Submission" where id = $1`, j.SubmissionId).Scan(&status)
	if err == pgx.ErrNoRows {
		return jobs.Done()
	}
	if err != nil {
		return jobs.RetryAt(time.Now().Add(time.Minute), j.Attempt, j.TimeoutRetries, err.Error())
	}
	// Only an open or second-pass-held, fully-captured ship refreshes: a decided /
	// parked / withdrawn snapshot is final, and 'processing' still belongs to the
	// ingest capture. A second-pass confirm re-settles from the evidence, so a
	// held ship's snapshot is still actionable for the verifying organizer.
	if status != "pending" && status != "secondpass" {
		return jobs.Done()
	}

	result, err := p.Recapture(ctx, j.SubmissionId)
	if err == nil && result.OK {
		return jobs.Done()
	}

	reason := "unknown"
	if err != nil {
		reason = err.Error()
	} else if len(result.Notes) > 0 {
		reason = result.Notes[0]
	}
	// The URL will never become fetchable; retrying is pure clone-pool load. The
	// ingest capture already vetted it once, so unlike Handle this never rejects -
	// the reviewer is looking at the ship and can judge the repository themselves.
	if err == nil && noteWith(result.Notes, "unsafe_url") != "" {
		slog.Warn("reenrich skipped an unsafe repo url", "submissionId", j.SubmissionId, "notes", strings.Join(result.Notes, ","))
		p.Reject.LogEvidenceIssue(ctx, j.SubmissionId, "capture", j.Attempt+1,
			noteWith(result.Notes, "unsafe_url"), result.Notes, true)
		return jobs.Done()
	}
	var notes []string
	if err == nil {
		notes = result.Notes
	}
	exhausted := j.Attempt >= len(retryDelays)
	// Mirrors Handle's audit trail so the review screen's sync warning stays
	// current after a reviewer-triggered recapture fails too.
	p.Reject.LogEvidenceIssue(ctx, j.SubmissionId, "capture", j.Attempt+1, reason, notes, exhausted)
	if exhausted {
		slog.Warn("reenrich gave up; the previous snapshot stays current",
			"submissionId", j.SubmissionId, "reason", reason)
		return jobs.Done()
	}
	return jobs.RetryAt(time.Now().Add(retryDelays[j.Attempt]), j.Attempt+1, j.TimeoutRetries, reason)
}
