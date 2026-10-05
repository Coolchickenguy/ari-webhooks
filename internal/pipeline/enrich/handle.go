package enrich

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hackclub/ari-webhooks/internal/autoreject"
	"github.com/hackclub/ari-webhooks/internal/jobs"
)

// The registration capture's normal retry window; a transient upstream failure
// at ingest must not freeze a thin snapshot forever.
var retryDelays = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute}

func noteWith(notes []string, code string) string {
	for _, n := range notes {
		if n == code || strings.HasPrefix(n, code+":") {
			return n
		}
	}
	return ""
}

// Handle is the job handler for kind "enrich": ari's enrichInBackground retry
// state machine on the durable job row. Attempt indexes the normal window;
// TimeoutRetries counts the separate clone-timeout loop.
func (p *Pipeline) Handle(ctx context.Context, j jobs.Job) jobs.Outcome {
	var status string
	err := p.Pool.QueryRow(ctx, `select status::text from "Submission" where id = $1`, j.SubmissionId).Scan(&status)
	if err == pgx.ErrNoRows {
		return jobs.Done()
	}
	if err != nil {
		return jobs.RetryAt(time.Now().Add(time.Minute), j.Attempt, j.TimeoutRetries, err.Error())
	}
	if status != "processing" {
		return jobs.Done() // captured, decided, or swept: the snapshot is final
	}

	result, err := p.Enrich(ctx, j.SubmissionId)
	if err != nil {
		slog.Error("enrich failed", "submissionId", j.SubmissionId, "attempt", j.Attempt, "err", err)
		exhausted := j.Attempt >= len(retryDelays)
		p.Reject.LogEvidenceIssue(ctx, j.SubmissionId, "capture", j.Attempt+1, err.Error(), nil, exhausted)
		if exhausted {
			if perr := p.promoteAndGate(ctx, j.SubmissionId); perr != nil {
				return jobs.RetryAt(time.Now().Add(time.Minute), j.Attempt, j.TimeoutRetries, perr.Error())
			}
			return jobs.Done()
		}
		return jobs.RetryAt(time.Now().Add(retryDelays[j.Attempt]), j.Attempt+1, j.TimeoutRetries, err.Error())
	}

	// A permanently unsafe repo URL will never be fixable; it can surface even
	// when ok is true (0 commits + skipped Hackatime reads as fullySynced).
	if unsafe := noteWith(result.Notes, "unsafe_url"); unsafe != "" {
		p.Reject.AutoReject(ctx, autoreject.Input{
			SubmissionId: j.SubmissionId,
			Reason:       autoreject.InaccessibleRepo,
			Detail:       unsafe,
		})
		return jobs.Done()
	}

	if result.OK {
		return jobs.Done()
	}
	slog.Warn("enrich incomplete", "submissionId", j.SubmissionId, "attempt", j.Attempt, "notes", strings.Join(result.Notes, ","))

	// A clone timeout or TLS-infra failure says nothing about the repo: never
	// auto-reject, never consume the normal window. Retry every 5-10 minutes
	// until it clones or the 6h stuck sweep rejects the ship.
	infraNote := noteWith(result.Notes, "clone_timeout")
	if infraNote == "" {
		infraNote = noteWith(result.Notes, "log_timeout")
	}
	if infraNote == "" {
		infraNote = noteWith(result.Notes, "clone_infra")
	}
	if infraNote == "" {
		infraNote = noteWith(result.Notes, "github_unavailable") // GitHub itself was down or throttling: says nothing about the repo either
	}
	if infraNote != "" {
		if j.TimeoutRetries == 0 { // one audit entry per timeout episode is enough
			p.Reject.LogEvidenceIssue(ctx, j.SubmissionId, "capture", j.Attempt+1,
				infraNote+" - retrying every ~5min until it clones or the 6h sweep rejects it", result.Notes, false)
		}
		delay := 5*time.Minute + time.Duration(rand.Int64N(int64(5*time.Minute))) // jitter so a burst never re-saturates the clone pool
		return jobs.RetryAt(time.Now().Add(delay), j.Attempt, j.TimeoutRetries+1, infraNote)
	}

	exhausted := j.Attempt >= len(retryDelays)
	firstNote := "unknown"
	if len(result.Notes) > 0 {
		firstNote = result.Notes[0]
	}
	p.Reject.LogEvidenceIssue(ctx, j.SubmissionId, "capture", j.Attempt+1, firstNote, result.Notes, exhausted)

	if cloneFailed := noteWith(result.Notes, "clone_failed"); cloneFailed != "" {
		if exhausted { // a repo must be a cloneable git remote; a CAD/document link is not a repo
			p.Reject.AutoReject(ctx, autoreject.Input{
				SubmissionId: j.SubmissionId,
				Reason:       autoreject.InaccessibleRepo,
				Detail:       cloneFailed,
			})
			return jobs.Done()
		}
		return jobs.RetryAt(time.Now().Add(retryDelays[j.Attempt]), j.Attempt+1, j.TimeoutRetries, cloneFailed)
	}
	if exhausted {
		// Partial evidence is as complete as it will get: promote so the ship
		// becomes visible in the review queue.
		if perr := p.promoteAndGate(ctx, j.SubmissionId); perr != nil {
			return jobs.RetryAt(time.Now().Add(time.Minute), j.Attempt, j.TimeoutRetries, perr.Error())
		}
		return jobs.Done()
	}
	return jobs.RetryAt(time.Now().Add(retryDelays[j.Attempt]), j.Attempt+1, j.TimeoutRetries, firstNote)
}

// promoteAndGate flips a fully-processed ship to 'pending' and tells the
// fraud gateway. It returns the promotion error so the caller can retry rather than
// settle the job Done over a transient failure — otherwise the ship would sit in
// 'processing' until the 6h stuck sweep wrongly rejects it.
func (p *Pipeline) promoteAndGate(ctx context.Context, submissionId string) error {
	tag, err := p.Pool.Exec(ctx,
		`update "Submission" set status = 'pending' where id = $1 and status = 'processing'`, submissionId)
	if err != nil {
		slog.Error("promote failed", "submissionId", submissionId, "err", err)
		return err
	}
	if tag.RowsAffected() > 0 {
		p.Fraud.OnEnteredQueue(ctx, submissionId)
	}
	return nil
}
