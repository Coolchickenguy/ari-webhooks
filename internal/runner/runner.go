package runner

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hackclub/ari-webhooks/internal/db"
)

type Runner struct {
	// Connection string for the per-sweep advisory-lock session. Deliberately NOT
	// the shared pool: a sweep holds its lock connection for the whole run, and
	// every sweep body goes back to the pool for its own queries. Taking the lock
	// from the pool meant N concurrent sweeps pinned N pool connections while
	// waiting on work that needed more of them - with enough sweeps overlapping,
	// every connection was held by a sweep whose body was blocked forever in
	// Acquire (the sweep context has no deadline, so it never errors out). That
	// deadlocked the whole service silently: HTTP kept serving non-DB routes, the
	// pod stayed Ready, and nothing was logged. Prod outages 2026-07-21 and
	// 2026-07-27, the latter for 41h. Same dedicated-connection shape the outbound
	// listener already uses.
	connString string
	wg         sync.WaitGroup
}

func New(connString string) *Runner {
	return &Runner{connString: connString}
}

type Sweep struct {
	Name      string
	Interval  time.Duration
	Jitter    time.Duration
	LockKey   int64
	Immediate bool
	Fn        func(ctx context.Context) error
}

func (r *Runner) Every(ctx context.Context, s Sweep) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		if s.Immediate {
			r.runSweep(ctx, s)
		}
		for {
			delay := s.Interval
			if s.Jitter > 0 {
				delay += time.Duration(rand.Int64N(int64(s.Jitter)))
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			r.runSweep(ctx, s)
		}
	}()
}

func (r *Runner) runSweep(ctx context.Context, s Sweep) {
	defer func() {
		if p := recover(); p != nil { // one bad row must never kill the loop
			slog.Error("sweep panic", "sweep", s.Name, "panic", p, "stack", string(debug.Stack()))
		}
	}()

	// Its own session, so the lock this holds for the length of the run costs the
	// shared pool nothing. Closing the connection ends the session and drops the
	// advisory lock even if the explicit unlock below never runs.
	conn, err := pgx.Connect(ctx, db.SanitizeUrl(r.connString))
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("sweep could not get a connection", "sweep", s.Name, "err", err)
		}
		return
	}
	defer conn.Close(context.WithoutCancel(ctx))

	var locked bool
	if err := conn.QueryRow(ctx, "select pg_try_advisory_lock($1)", s.LockKey).Scan(&locked); err != nil {
		slog.Warn("sweep lock query failed", "sweep", s.Name, "err", err)
		return
	}
	if !locked {
		return // another instance is sweeping
	}
	defer conn.Exec(context.WithoutCancel(ctx), "select pg_advisory_unlock($1)", s.LockKey)

	start := time.Now()
	if err := s.Fn(ctx); err != nil {
		slog.Warn("sweep failed", "sweep", s.Name, "err", err, "durationMs", time.Since(start).Milliseconds())
		return
	}
	slog.Debug("sweep done", "sweep", s.Name, "durationMs", time.Since(start).Milliseconds())
}

// Go runs fn as a tracked goroutine (used by workers that are not periodic sweeps).
func (r *Runner) Go(fn func()) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		fn()
	}()
}

func (r *Runner) Wait() {
	r.wg.Wait()
}
