package httpapi

import (
	"context"
	"testing"

	"github.com/hackclub/ari-webhooks/internal/ids"
)

// TestInternalReenrichProgramQueuesEveryPendingShip: the bulk trigger queues a
// reenrich job per pending ship, skips settled ones, and repeats as a no-op
// while those jobs are still live.
func TestInternalReenrichProgramQueuesEveryPendingShip(t *testing.T) {
	pool, app, subId := shipFixture(t, nil)
	ctx := context.Background()
	var programId, makerId string
	if err := pool.QueryRow(ctx,
		`select "programId", "makerId" from "Submission" where id = $1`, subId).Scan(&programId, &makerId); err != nil {
		t.Fatal(err)
	}
	pendingTwin := ids.ShipId()
	if _, err := pool.Exec(ctx, `
		insert into "Submission" (id, "programId", "externalId", "makerId", title, "repoUrl", "claimedHours", status)
		values ($1, $2, 'ext-2', $3, 'T2', 'https://github.com/a/c', 0, 'pending'),
		       ($4, $2, 'ext-3', $3, 'T3', 'https://github.com/a/d', 0, 'approved')`,
		pendingTwin, programId, makerId, ids.ShipId()); err != nil {
		t.Fatal(err)
	}

	code, body := callInternal(t, app, "POST", "/internal/reenrich-program/"+programId, "sekrit", "")
	if code != 200 || body["ok"] != true {
		t.Fatalf("got %d %v", code, body)
	}
	if body["queued"] != float64(2) {
		t.Fatalf("both pending ships and only them must be covered: %v", body["queued"])
	}
	var jobs int
	if err := pool.QueryRow(ctx, `
		select count(*) from ariw.job where kind = 'reenrich' and status = 'due'
		and "submissionId" in ($1, $2)`, subId, pendingTwin).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 2 {
		t.Fatalf("a due reenrich job per pending ship: %d rows", jobs)
	}

	// A second trigger while the jobs are live must not duplicate them.
	if code, body = callInternal(t, app, "POST", "/internal/reenrich-program/"+programId, "sekrit", ""); code != 200 || body["ok"] != true {
		t.Fatalf("repeat trigger: %d %v", code, body)
	}
	if err := pool.QueryRow(ctx,
		`select count(*) from ariw.job where kind = 'reenrich'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 2 {
		t.Fatalf("re-triggering must be idempotent while jobs are live: %d rows", jobs)
	}

	if code, _ := callInternal(t, app, "POST", "/internal/reenrich-program/nope", "sekrit", ""); code != 404 {
		t.Fatalf("unknown program: got %d, want 404", code)
	}
}
