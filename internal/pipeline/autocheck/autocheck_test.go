package autocheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hackclub/ari-webhooks/internal/autoreject"
	"github.com/hackclub/ari-webhooks/internal/cryptobox"
	"github.com/hackclub/ari-webhooks/internal/ids"
	"github.com/hackclub/ari-webhooks/internal/integrations/probe"
	"github.com/hackclub/ari-webhooks/internal/jobs"
	"github.com/hackclub/ari-webhooks/internal/outbound"
	"github.com/hackclub/ari-webhooks/internal/testdb"
)

func setup(t *testing.T, demoUrl string) (*pgxpool.Pool, *Pipeline, string) {
	t.Helper()
	probe.AllowPrivateHosts = true // httptest listens on 127.0.0.1
	t.Cleanup(func() { probe.AllowPrivateHosts = false })
	pool := testdb.New(t)
	ctx := context.Background()
	programId := ids.Cuid()
	makerId := ids.Cuid()
	subId := ids.ShipId()
	if _, err := pool.Exec(ctx, `insert into "Program" (id, name, color) values ($1, 'P', '#000')`, programId); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into "Maker" (id, email) values ($1, 'm@x.com')`, makerId); err != nil {
		t.Fatal(err)
	}
	var demo any
	if demoUrl != "" {
		demo = demoUrl
	}
	if _, err := pool.Exec(ctx, `
		insert into "Submission" (id, "programId", "externalId", "makerId", title, "repoUrl", "demoUrl", "claimedHours", status)
		values ($1, $2, 'ext', $3, 'T', 'https://github.com/a/b', $4, 0, 'processing')`,
		subId, programId, makerId, demo); err != nil {
		t.Fatal(err)
	}
	codec, _ := cryptobox.New(make([]byte, 32))
	reject := &autoreject.Service{Pool: pool, Outbound: outbound.NewWorker(pool, codec, "test")}
	return pool, &Pipeline{Pool: pool, Reject: reject}, subId
}

func status(t *testing.T, pool *pgxpool.Pool, subId string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `select status::text from "Submission" where id = $1`, subId).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLiveDemoIsDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	pool, p, subId := setup(t, srv.URL)
	if o := p.Handle(context.Background(), jobs.Job{SubmissionId: subId}); !o.IsDone() {
		t.Fatalf("live demo must finish the job: %+v", o)
	}
	if got := status(t, pool, subId); got != "processing" {
		t.Fatalf("status must be untouched: %s", got)
	}
}

func TestDeadDemoWalksFullWindowThenRejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()
	pool, p, subId := setup(t, srv.URL)
	ctx := context.Background()

	// Attempts 0..2 retry with the ari delays; even a hard 404 never rejects early.
	wantDelays := []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute}
	for attempt := 0; attempt < 3; attempt++ {
		before := time.Now()
		o := p.Handle(ctx, jobs.Job{SubmissionId: subId, Attempt: attempt})
		if !o.IsRetry() || o.NextAttempt() != attempt+1 {
			t.Fatalf("attempt %d must retry: %+v", attempt, o)
		}
		delay := o.RetryTime().Sub(before)
		if delay < wantDelays[attempt]-5*time.Second || delay > wantDelays[attempt]+5*time.Second {
			t.Fatalf("attempt %d delay %v, want ~%v", attempt, delay, wantDelays[attempt])
		}
	}
	if o := p.Handle(ctx, jobs.Job{SubmissionId: subId, Attempt: 3}); !o.IsDone() {
		t.Fatalf("final attempt must settle the job: %+v", o)
	}
	if got := status(t, pool, subId); got != "rejected" {
		t.Fatalf("exhausted dead demo must auto-reject: %s", got)
	}
	var note string
	if err := pool.QueryRow(ctx, `select "noteToMaker" from "Review" where "submissionId" = $1`, subId).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if note != "We couldn't reach your demo URL, so this submission was auto-rejected. Make sure the demo is live and publicly accessible, then resubmit." {
		t.Fatalf("maker note is program-visible contract: %q", note)
	}
	var evidenceEvents int
	pool.QueryRow(ctx, `select count(*) from "ActivityEvent" where kind = 'EVIDENCE' and meta->>'source' = 'demo'`).Scan(&evidenceEvents)
	if evidenceEvents != 4 {
		t.Fatalf("every failed probe must be audited: %d", evidenceEvents)
	}
}

func TestBotWalledDemoIsLeftForHumans(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer srv.Close()
	pool, p, subId := setup(t, srv.URL)
	ctx := context.Background()

	if o := p.Handle(ctx, jobs.Job{SubmissionId: subId, Attempt: 3}); !o.IsDone() {
		t.Fatal("blocked final attempt must settle")
	}
	if got := status(t, pool, subId); got != "processing" {
		t.Fatalf("bot-walled demo must never auto-reject: %s", got)
	}
	var noted int
	pool.QueryRow(ctx,
		`select count(*) from "ActivityEvent" where kind = 'EVIDENCE' and meta->'notes'->>0 like '%left for human review%'`).Scan(&noted)
	if noted != 1 {
		t.Fatalf("the block must be documented in the feed: %d", noted)
	}
}

func TestNoDemoUrlIsDone(t *testing.T) {
	pool, p, subId := setup(t, "")
	if o := p.Handle(context.Background(), jobs.Job{SubmissionId: subId}); !o.IsDone() {
		t.Fatal("no demo url declared must be a no-op")
	}
	if got := status(t, pool, subId); got != "processing" {
		t.Fatalf("status: %s", got)
	}
}

// Bot-walled hosts never take the probe path at all - Printables kills the
// connection with no HTTP status (so exhausting the window false auto-rejects)
// and Steam refuses every datacenter attempt. The reviewer opens the link anyway.
func TestUnprobeableDemoHostMatching(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://www.printables.com/model/12345-thing", true},
		{"https://printables.com/model/12345-thing", true},
		{"https://steamcommunity.com/sharedfiles/filedetails/?id=3763024921", true},
		{"https://STEAMCOMMUNITY.com/sharedfiles/filedetails/?id=1", true},
		{"https://www.steamcommunity.com/sharedfiles/filedetails/?id=1", true},
		{"https://example.com/", false},
		{"https://notprintables.com/", false},
		{"https://printables.com.evil.com/", false},
		{"://not-a-url", false},
	}
	for _, tc := range cases {
		if got := isUnprobeableDemoHost(tc.url); got != tc.want {
			t.Errorf("isUnprobeableDemoHost(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestUnprobeableDemoHostSkipsProbe(t *testing.T) {
	// No httptest server: the job must finish without any network attempt.
	pool, p, subId := setup(t, "https://steamcommunity.com/sharedfiles/filedetails/?id=3763024921")
	ctx := context.Background()
	if o := p.Handle(ctx, jobs.Job{SubmissionId: subId}); !o.IsDone() {
		t.Fatalf("bot-walled host must skip the probe: %+v", o)
	}
	if got := status(t, pool, subId); got != "processing" {
		t.Fatalf("status must be untouched: %s", got)
	}
	var evidenceEvents int
	pool.QueryRow(ctx, `select count(*) from "ActivityEvent" where kind = 'EVIDENCE'`).Scan(&evidenceEvents)
	if evidenceEvents != 0 {
		t.Fatalf("skipping must not spam the activity feed: %d", evidenceEvents)
	}
}
