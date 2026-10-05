package sweep

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hackclub/ari-webhooks/internal/httpx"
	"github.com/hackclub/ari-webhooks/internal/ids"
	"github.com/hackclub/ari-webhooks/internal/integrations/vmplat"
)

type VmReaper struct {
	Pool *pgxpool.Pool
	Vm   *vmplat.Client
}

func (r *VmReaper) killOrTombstone(ctx context.Context, vmid int) {
	if r.Vm.DeleteVm(ctx, vmid) {
		if _, err := r.Pool.Exec(ctx, `delete from "VmTombstone" where vmid = $1`, vmid); err != nil {
			slog.Warn("tombstone cleanup failed", "vmid", vmid, "err", err)
		}
		return
	}
	_, err := r.Pool.Exec(ctx, `
		insert into "VmTombstone" (vmid) values ($1)
		on conflict (vmid) do update set attempts = "VmTombstone".attempts + 1, "lastTriedAt" = now()`,
		vmid)
	if err != nil {
		slog.Warn("could not tombstone vm", "vmid", vmid, "err", err)
	}
}

// ReapStale tears down reviewer VMs whose owner went idle for an hour or that
// passed the platform's 3h hard cap. The delete uses RETURNING so audit events
// are written only for rows this sweep actually removed; a concurrent sweep
// (ari's, another replica) can never double-log.
func (r *VmReaper) ReapStale(ctx context.Context) error {
	if !r.Vm.Configured() {
		return nil
	}
	rows, err := r.Pool.Query(ctx, `
		delete from "ReviewerVm" v
		using "User" u
		where u.id = v."reviewerId"
		  and (u."lastSeenAt" < now() - interval '1 hour' or v."createdAt" < now() - interval '3 hours')
		returning v.vmid, v."vmType", v."programId", v."submissionId", v."reviewerId",
		          v."createdAt" < now() - interval '3 hours'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type reaped struct {
		vmid                                    int
		vmType, programId, submissionId, userId string
		expired                                 bool
	}
	var stale []reaped
	for rows.Next() {
		var s reaped
		if err := rows.Scan(&s.vmid, &s.vmType, &s.programId, &s.submissionId, &s.userId, &s.expired); err != nil {
			return err
		}
		stale = append(stale, s)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(stale) == 0 {
		return nil
	}

	for _, s := range stale {
		// "expired" wins the label: past 3h the machine is definitely gone.
		text := "Idle review VM auto-deleted (1h inactive)"
		reason := "idle"
		if s.expired {
			text = "Review VM auto-deleted (3h max lifetime reached)"
			reason = "expired"
		}
		_, err := r.Pool.Exec(ctx, `
			insert into "ActivityEvent" (id, "programId", kind, "actorId", "submissionId", text, meta)
			values ($1, $2, 'VM'::"ActivityKind", $3, $4, $5, $6)`,
			ids.Cuid(), s.programId, s.userId, s.submissionId, text,
			map[string]any{"op": "reap", "reason": reason, "type": s.vmType, "vmid": s.vmid})
		if err != nil {
			slog.Warn("vm reap audit failed", "vmid", s.vmid, "err", err)
		}
	}

	httpx.MapLimit(ctx, stale, 4, func(ctx context.Context, s reaped, _ int) struct{} {
		r.killOrTombstone(ctx, s.vmid)
		return struct{}{}
	})
	slog.Info("reaped stale reviewer vms", "count", len(stale))
	return nil
}

// ReapTombstones retries every VM whose delete the platform never confirmed.
func (r *VmReaper) ReapTombstones(ctx context.Context) error {
	if !r.Vm.Configured() {
		return nil
	}
	rows, err := r.Pool.Query(ctx, `select vmid from "VmTombstone" limit 100`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var dead []int
	for rows.Next() {
		var vmid int
		if err := rows.Scan(&vmid); err != nil {
			return err
		}
		dead = append(dead, vmid)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(dead) == 0 {
		return nil
	}
	httpx.MapLimit(ctx, dead, 4, func(ctx context.Context, vmid, _ int) struct{} {
		r.killOrTombstone(ctx, vmid)
		return struct{}{}
	})
	slog.Info("retried tombstoned vms", "count", len(dead))
	return nil
}
