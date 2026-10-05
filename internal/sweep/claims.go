package sweep

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ReapStaleClaims releases soft review locks that are idle past the 30-minute
// TTL or sit on ships that are no longer open.
func ReapStaleClaims(ctx context.Context, pool *pgxpool.Pool) error {
	// Every claim about to be cleared gets its live review session closed on the
	// timeline first. Idle claims on open ships close as 'idle' (the reviewer
	// walked away; the ship goes back up for grabs). Claims on ships that left
	// 'pending' close as 'left': the decision paths close sessions themselves,
	// but a path that misses one would otherwise strand the row open forever,
	// because once the claim is gone nothing else can find it.
	rows, err := pool.Query(ctx, `
		select id, "claimedById", status = 'pending' from "Submission"
		where "claimedById" is not null
		  and ("claimedAt" < now() - interval '30 minutes' or status != 'pending')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type held struct {
		submissionId, reviewerId string
		open                     bool
	}
	var stale []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.submissionId, &h.reviewerId, &h.open); err != nil {
			return err
		}
		stale = append(stale, h)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, h := range stale {
		reason := "left"
		if h.open {
			reason = "idle"
		}
		// Idempotent stamp of the latest still-open session; a session already
		// closed by another path (or another instance) is a no-op.
		_, err := pool.Exec(ctx, `
			update "SubmissionOpen" set "closedAt" = now(), "closeReason" = $3
			where id = (
				select id from "SubmissionOpen"
				where "submissionId" = $1 and "reviewerId" = $2 and "closedAt" is null
				order by "openedAt" desc limit 1
			)`,
			h.submissionId, h.reviewerId, reason)
		if err != nil {
			slog.Warn("close open session failed", "submissionId", h.submissionId, "err", err)
		}
	}

	tag, err := pool.Exec(ctx, `
		update "Submission" set "claimedById" = null, "claimedAt" = null
		where "claimedById" is not null
		  and ("claimedAt" < now() - interval '30 minutes' or status != 'pending')`)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		slog.Info("released stale review claims", "count", tag.RowsAffected())
	}
	return nil
}
