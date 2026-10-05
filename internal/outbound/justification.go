package outbound

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Override Hours Spent Justification (YSWS handbook, Quality and Integrity): the
// internal evidence record behind the hours a program credits. Field names and the
// comma-separated string shapes are the handbook's, not ours - receivers store these
// values as they arrive.
//
// ari builds the same block for reviewer decisions (src/lib/server/justification.ts);
// this is the copy for the events this service sends on its own (system
// decisions and relays), so a program sees one payload shape whoever sent it. Both
// sides honour the same per-program opt-in, Program.hoursJustification.
//
// The handbook's other two fields, technical_features and deflation_reason, are prose
// a reviewer writes as part of a decision, and ari sends them from that decision. Every
// event from here is a system event that carries none, so they are ari's alone: reading
// them off the ship instead would attach a first-pass reviewer's words to a later system
// event, which credits no hours and asked nobody anything.
type justificationBlock struct {
	HackatimeProjects string `json:"hackatime_projects,omitempty"`
	HackatimeUserId   string `json:"hackatime_user_id,omitempty"`
	LapseLinks        string `json:"lapse_links,omitempty"`
}

func (j justificationBlock) empty() bool {
	return j == justificationBlock{}
}

// loadJustification assembles the block for a submission out of the evidence the
// capture recorded, for programs that asked for one.
func (w *Worker) loadJustification(ctx context.Context, submissionId string) (*justificationBlock, error) {
	var wanted bool
	var projects []string
	var receivedAt time.Time
	var hackatimeUserId *string
	var trackingFromAt *time.Time
	err := w.Pool.QueryRow(ctx, `
		select p."hoursJustification", s."hackatimeProjects", s."receivedAt", m."hackatimeUserId",
		       h."trackingFromAt"
		from "Submission" s
		join "Program" p on p.id = s."programId"
		join "Maker" m on m.id = s."makerId"
		left join "HoursBreakdown" h on h."submissionId" = s.id
		where s.id = $1`,
		submissionId).Scan(&wanted, &projects, &receivedAt, &hackatimeUserId, &trackingFromAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !wanted {
		return nil, nil // the program runs the legacy flow, so its payloads carry no justification
	}

	var block justificationBlock
	var named []string
	for _, p := range projects {
		if p = strings.TrimSpace(p); p != "" {
			named = append(named, p)
		}
	}
	if len(named) > 0 {
		// One evidence window covers the whole ship, so every project carries the same
		// range. An unknown window start yields bare names rather than a plausible date
		// range that was never analyzed.
		suffix := ""
		if trackingFromAt != nil {
			suffix = " " + handbookDate(*trackingFromAt) + "-" + handbookDate(receivedAt)
		}
		for i, p := range named {
			named[i] = p + suffix
		}
		block.HackatimeProjects = strings.Join(named, ", ")
		// Only meaningful next to the projects: the id exists so a spot-checker can pull
		// up the same heartbeats the reviewer read.
		if hackatimeUserId != nil {
			block.HackatimeUserId = strings.TrimSpace(*hackatimeUserId)
		}
	}

	links, err := w.lapseLinks(ctx, submissionId)
	if err != nil {
		return nil, err
	}
	block.LapseLinks = strings.Join(links, ", ")

	if block.empty() {
		return nil, nil
	}
	return &block, nil
}

func (w *Worker) lapseLinks(ctx context.Context, submissionId string) ([]string, error) {
	rows, err := w.Pool.Query(ctx,
		`select url from "ElapsedClip" where "submissionId" = $1 and url is not null order by at`,
		submissionId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			return nil, err
		}
		if url = strings.TrimSpace(url); url != "" {
			out = append(out, url)
		}
	}
	return out, rows.Err()
}

// handbookDate renders M/D/YYYY, the handbook's format. UTC, matching the UTC-day
// buckets the evidence window and Hackatime span accounting are built on - the
// server's local zone would shift a range by a day near midnight.
func handbookDate(t time.Time) string {
	return t.UTC().Format("1/2/2006")
}
