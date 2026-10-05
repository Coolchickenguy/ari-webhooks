// drives one ship through the wired service: signed ingest over http, then
// the job queue, against a throwaway database
package apptest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hackclub/ari-webhooks/internal/app"
	"github.com/hackclub/ari-webhooks/internal/config"
	"github.com/hackclub/ari-webhooks/internal/cryptobox"
	"github.com/hackclub/ari-webhooks/internal/ids"
	"github.com/hackclub/ari-webhooks/internal/pipeline/enrich/enrichtest"
	"github.com/hackclub/ari-webhooks/internal/testdb"
)

type Run struct {
	Pool         *pgxpool.Pool
	Core         *app.App
	Http         *fiber.App
	ProgramId    string
	SubmissionId string
	Status       string
}

// waits for the enrich and screen jobs. configure adjusts the config before wiring
func ShipReachesReview(t *testing.T, configure func(cfg *config.Config)) Run {
	t.Helper()
	pool := testdb.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	codec, err := cryptobox.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}

	programId := ids.Cuid()
	if _, err := pool.Exec(ctx, `
		insert into "Program" (id, name, color, accepts)
		values ($1, 'Test Program', '#123456', array['commits','devlog']::"Evidence"[])`, programId); err != nil {
		t.Fatal(err)
	}
	secret := "whsec_app-test"
	enc, err := codec.Encrypt(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into "WebhookSecret" (id, "programId", last4, "secretEnc") values ($1, $2, $3, $4)`,
		ids.Cuid(), programId, secret[len(secret)-4:], enc); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{WorkerId: "test", GitCloneConcurrency: 2, JobWorkers: 2, InternalApiToken: "sekrit"}
	if configure != nil {
		configure(cfg)
	}
	core := app.New(cfg, pool, codec)
	core.CaptureGit.AllowPrivateHosts = true // the test repository is served from 127.0.0.1
	core.RegisterJobs()
	done := make(chan struct{})
	go func() {
		defer close(done)
		core.Queue.Run(ctx, 100*time.Millisecond, 2)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	body, _ := json.Marshal(map[string]any{
		"external_id":   "p1",
		"maker":         map[string]any{"email": "mia@example.com", "name": "Mia", "slack_id": "U123"},
		"title":         "My Project",
		"description":   "A thing I made",
		"repo_url":      enrichtest.ServeRepoWith(t, map[string]string{"README.md": "# hi\n"}),
		"demo_url":      "https://proj.example.com", // its probe is queued five seconds out, after this run has ended
		"thumbnail_url": "https://img.example.com/t.png",
		"evidence":      []string{"commits", "devlog"},
		"journals":      []map[string]any{{"at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), "seconds": 5400, "text": "built it"}},
	})
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	http := core.Server().App()
	req := httptest.NewRequest("POST", "/api/ingest/"+programId, strings.NewReader(string(body)))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-ari-signature", hex.EncodeToString(mac.Sum(nil)))
	res, err := http.Test(req, fiber.TestConfig{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var accepted map[string]any
	if err := json.NewDecoder(res.Body).Decode(&accepted); err != nil {
		t.Fatal(err)
	}
	submissionId, _ := accepted["id"].(string)
	if res.StatusCode != 202 || submissionId == "" {
		t.Fatalf("ingest: %d %v", res.StatusCode, accepted)
	}

	run := Run{Pool: pool, Core: core, Http: http, ProgramId: programId, SubmissionId: submissionId}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var liveJobs int
		if err := pool.QueryRow(ctx, `
			select s.status::text,
			       (select count(*) from ariw.job j where j."submissionId" = s.id and j.kind in ('enrich', 'screen') and j.status in ('due', 'running'))
			from "Submission" s where s.id = $1`, submissionId).Scan(&run.Status, &liveJobs); err != nil {
			t.Fatal(err)
		}
		if run.Status != "processing" && liveJobs == 0 {
			return run
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the ship never left processing: %s", run.Status)
	return run
}
