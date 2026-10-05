package outbound

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hackclub/ari-webhooks/internal/db"
)

// Channel the frozen-repo cutover will pg_notify with the delivery id as payload.
const NotifyChannel = "outboundDeliveryPending"

// Listen reconnects with jittered backoff; after any gap the worker is woken so a
// poll pass covers notifications lost while the listener was down.
func (w *Worker) Listen(ctx context.Context, connString string) {
	for ctx.Err() == nil {
		if err := w.listenOnce(ctx, connString); err != nil && ctx.Err() == nil {
			slog.Warn("outbound listener disconnected", "err", err)
		}
		w.Wake() // cover anything missed while the listener was down
		select {
		case <-ctx.Done():
			return
		case <-time.After(2*time.Second + time.Duration(rand.Int64N(int64(3*time.Second)))):
		}
	}
}

func (w *Worker) listenOnce(ctx context.Context, connString string) error {
	conn, err := pgx.Connect(ctx, db.SanitizeUrl(connString))
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if _, err := conn.Exec(ctx, `listen "`+NotifyChannel+`"`); err != nil {
		return err
	}
	w.Wake() // a poll pass right after (re)subscribing closes the gap window
	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if notification.Payload == "" {
			w.Wake()
			continue
		}
		// Schedule for immediate claim; keep an earlier nextAttemptAt and any live lease.
		_, err = w.Pool.Exec(ctx, `
			insert into ariw."outboundSchedule" ("deliveryId", "nextAttemptAt")
			select $1, now()
			where exists (select 1 from "OutboundDelivery" where id = $1 and status = 'PENDING')
			on conflict ("deliveryId") do update
				set "nextAttemptAt" = least(ariw."outboundSchedule"."nextAttemptAt", excluded."nextAttemptAt")`,
			notification.Payload)
		if err != nil {
			slog.Warn("notify schedule failed", "deliveryId", notification.Payload, "err", err)
		}
		w.Wake()
	}
}
