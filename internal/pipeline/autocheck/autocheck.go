package autocheck

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hackclub/ari-webhooks/internal/autoreject"
	"github.com/hackclub/ari-webhooks/internal/integrations/probe"
	"github.com/hackclub/ari-webhooks/internal/jobs"
)

type Pipeline struct {
	Pool   *pgxpool.Pool
	Reject *autoreject.Service
}

// Delays between demo probes after the first at ingest+5s; every failure walks
// the full window before any verdict (a 404 five seconds after submit is
// routinely a deploy still propagating).
var checkDelays = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute}

// Hosts whose bot protection refuses our probe from a datacenter IP while the
// page is perfectly alive in a browser - Printables kills the connection with
// no HTTP status (so the blocked 401/403/429 escape hatch never sees it and
// the exhausted window false auto-rejects), Steam answers every attempt with a
// bot-wall error. Probing them can only ever fail, so skip the check and leave
// the link to the human reviewer, who opens it anyway.
var unprobeableDemoHosts = map[string]bool{
	"printables.com":     true,
	"steamcommunity.com": true,
}

func isUnprobeableDemoHost(rawUrl string) bool {
	u, err := url.Parse(rawUrl)
	if err != nil {
		return false // unparseable URLs take the normal probe path
	}
	host := strings.ToLower(u.Hostname())
	return unprobeableDemoHosts[host] || unprobeableDemoHosts[strings.TrimPrefix(host, "www.")]
}

func (p *Pipeline) Handle(ctx context.Context, j jobs.Job) jobs.Outcome {
	var status string
	var demoUrl *string
	err := p.Pool.QueryRow(ctx,
		`select status::text, "demoUrl" from "Submission" where id = $1`, j.SubmissionId).
		Scan(&status, &demoUrl)
	if err == pgx.ErrNoRows {
		return jobs.Done()
	}
	if err != nil {
		return jobs.RetryAt(time.Now().Add(time.Minute), j.Attempt, j.TimeoutRetries, err.Error())
	}
	if status != "processing" && status != "pending" {
		return jobs.Done()
	}
	if demoUrl == nil || *demoUrl == "" {
		return jobs.Done()
	}
	if isUnprobeableDemoHost(*demoUrl) {
		return jobs.Done() // known bot-walled host: a human decides
	}

	result := probe.Url(ctx, *demoUrl)
	if result.OK {
		return jobs.Done()
	}
	slog.Warn("demo probe failed", "submissionId", j.SubmissionId, "attempt", j.Attempt, "error", result.Error)

	isLastAttempt := j.Attempt >= len(checkDelays)
	var notes []string
	if isLastAttempt && result.Blocked {
		notes = []string{"host is up but refused the probe (auth/bot wall) - left for human review"}
	}
	p.Reject.LogEvidenceIssue(ctx, j.SubmissionId, "demo", j.Attempt+1, result.Error, notes, isLastAttempt)

	if !isLastAttempt {
		return jobs.RetryAt(time.Now().Add(checkDelays[j.Attempt]), j.Attempt+1, j.TimeoutRetries, result.Error)
	}
	if result.Blocked {
		return jobs.Done() // alive but refusing the probe: a human decides
	}
	p.Reject.AutoReject(ctx, autoreject.Input{
		SubmissionId: j.SubmissionId,
		Reason:       autoreject.InaccessibleDemo,
		Detail:       result.Error,
	})
	return jobs.Done()
}
