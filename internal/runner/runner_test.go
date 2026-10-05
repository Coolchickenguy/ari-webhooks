package runner

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hackclub/ari-webhooks/internal/testdb"
)

func TestEverySweepsAndSurvivesPanic(t *testing.T) {
	pool := testdb.New(t)
	r := New(testdb.ConnString(pool))
	ctx, cancel := context.WithCancel(context.Background())

	var runs atomic.Int32
	r.Every(ctx, Sweep{
		Name: "panicky", Interval: 20 * time.Millisecond, LockKey: 101, Immediate: true,
		Fn: func(ctx context.Context) error {
			if runs.Add(1) == 1 {
				panic("first run explodes")
			}
			return nil
		},
	})

	deadline := time.Now().Add(10 * time.Second)
	for runs.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("sweep did not keep running after a panic: %d runs", runs.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	r.Wait()
}

func TestAdvisoryLockSkipsConcurrentSweep(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, "select pg_try_advisory_lock($1)", int64(202)).Scan(&locked); err != nil || !locked {
		t.Fatalf("could not take the lock for the test: %v", err)
	}

	r := New(testdb.ConnString(pool))
	var runs atomic.Int32
	sweepCtx, cancel := context.WithCancel(ctx)
	r.Every(sweepCtx, Sweep{
		Name: "held", Interval: 10 * time.Millisecond, LockKey: 202, Immediate: true,
		Fn: func(ctx context.Context) error {
			runs.Add(1)
			return nil
		},
	})
	time.Sleep(300 * time.Millisecond)
	if runs.Load() != 0 {
		t.Fatalf("sweep ran %d times while another instance held the lock", runs.Load())
	}

	if _, err := conn.Exec(ctx, "select pg_advisory_unlock($1)", int64(202)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for runs.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("sweep never ran after the lock was released")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	r.Wait()
}

// Regression: a running sweep must not consume a pool connection. Sweeps hold
// their advisory lock for the whole run while their bodies go back to the pool,
// so taking that lock FROM the pool let enough concurrent sweeps pin every
// connection and then deadlock waiting for one. That wedged prod on 2026-07-21
// and again on 2026-07-27 (41h): no crash, no log, pod still Ready.
//
// Saturate: start exactly MaxConns sweeps, hold them all inside Fn at once, and
// have each then query the pool. Against the old pool.Acquire implementation
// every connection is held by a sweep whose body is blocked in Acquire, and this
// times out.
func TestSweepsDoNotConsumePoolConnections(t *testing.T) {
	pool := testdb.New(t)
	n := int(pool.Config().MaxConns)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := New(testdb.ConnString(pool))
	entered := make(chan struct{}, n)
	release := make(chan struct{})
	var queried atomic.Int32

	for i := 0; i < n; i++ {
		r.Every(ctx, Sweep{
			// Distinct lock keys: this is about pool capacity, not lock contention.
			Name: "saturate", Interval: time.Hour, LockKey: int64(900 + i), Immediate: true,
			Fn: func(ctx context.Context) error {
				entered <- struct{}{}
				<-release // every sweep is now mid-run simultaneously
				var one int
				if err := pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
					return err
				}
				queried.Add(1)
				return nil
			},
		})
	}

	for i := 0; i < n; i++ {
		select {
		case <-entered:
		case <-time.After(30 * time.Second):
			t.Fatalf("only %d of %d sweeps started", i, n)
		}
	}
	close(release)

	deadline := time.Now().Add(30 * time.Second)
	for int(queried.Load()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("sweeps starved the pool: %d of %d bodies got a connection", queried.Load(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	r.Wait()
}
