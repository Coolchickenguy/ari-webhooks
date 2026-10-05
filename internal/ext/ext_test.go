package ext

import (
	"context"
	"testing"

	"github.com/hackclub/ari-webhooks/internal/jobs"
)

func TestNoopModuleFillsEverySeam(t *testing.T) {
	module := Load(Deps{})
	ctx := context.Background()
	if !module.Screener.Screen(ctx, jobs.Job{SubmissionId: "x"}).IsDone() {
		t.Fatal("the no-op screen settles its job")
	}
	if notes, settled := module.FlagChecks.AfterCapture(ctx, Capture{SubmissionId: "x", Settle: true}); len(notes) != 0 || settled {
		t.Fatalf("the no-op checks say nothing and settle nothing: %v %v", notes, settled)
	}
	gathered, notes, err := module.Evidence.Gather(ctx, []string{"a@example.com"})
	if gathered != nil || len(notes) != 0 || err != nil {
		t.Fatalf("the no-op evidence gathers nothing: %v %v %v", gathered, notes, err)
	}
	if notes, err := module.Evidence.StoreFiles(ctx, nil, "x", nil); len(notes) != 0 || err != nil {
		t.Fatalf("no-op StoreFiles: %v %v", notes, err)
	}
	if notes, err := module.Evidence.Store(ctx, nil, "x", nil); len(notes) != 0 || err != nil {
		t.Fatalf("no-op Store: %v %v", notes, err)
	}
	module.Fraud.OnEnteredQueue(ctx, "x")
	module.Fraud.OnWithdrawn(ctx, "x")
	module.Fraud.OnDecided(ctx, "x")
	module.Fraud.OnReingested(ctx, "x", "y")
	module.Routes(nil, nil)
	if len(module.Jobs) != 0 || len(module.Sweeps) != 0 {
		t.Fatal("the no-op module adds no jobs and no sweeps")
	}
}
