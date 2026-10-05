package app_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/hackclub/ari-webhooks/internal/app/apptest"
	"github.com/hackclub/ari-webhooks/internal/ext"
)

func TestPublicBuildTakesAShipToReviewWithNothingPluggedIn(t *testing.T) {
	if ext.Registered() {
		t.Fatal("the public test binary must not carry a module")
	}
	run := apptest.ShipReachesReview(t, nil)
	ctx := context.Background()
	if run.Status != "pending" {
		t.Fatalf("a captured ship waits for review: %s", run.Status)
	}

	var commits, devlogSeconds, flags, fraudChecks, reviews, deadJobs int
	if err := run.Pool.QueryRow(ctx, `
		select (select count(*) from "Commit" where "submissionId" = $1),
		       (select "devlogSeconds" from "HoursBreakdown" where "submissionId" = $1),
		       (select count(*) from "Flag" where "submissionId" = $1),
		       (select count(*) from "FraudCheck" where "submissionId" = $1),
		       (select count(*) from "Review" where "submissionId" = $1),
		       (select count(*) from ariw.job where "submissionId" = $1 and status = 'dead')`,
		run.SubmissionId).Scan(&commits, &devlogSeconds, &flags, &fraudChecks, &reviews, &deadJobs); err != nil {
		t.Fatal(err)
	}
	if commits != 1 || devlogSeconds != 5400 {
		t.Fatalf("time capture: commits=%d devlogSeconds=%d", commits, devlogSeconds)
	}
	if flags != 0 || fraudChecks != 0 || reviews != 0 {
		t.Fatalf("nothing screens, flags or decides: flags=%d fraudChecks=%d reviews=%d", flags, fraudChecks, reviews)
	}
	if deadJobs != 0 {
		t.Fatalf("every queued job kind has a handler: %d dead", deadJobs)
	}
	if len(run.Core.Module.Jobs) != 0 || len(run.Core.Module.Sweeps) != 0 {
		t.Fatalf("no extra jobs or sweeps: %d %d", len(run.Core.Module.Jobs), len(run.Core.Module.Sweeps))
	}

	req := httptest.NewRequest("POST", "/api/fraud/sometoken", nil)
	res, err := run.Http.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 404 {
		t.Fatalf("a route only a module serves does not exist here: %d", res.StatusCode)
	}
}
