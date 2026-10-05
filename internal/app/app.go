// the wiring main starts, kept here so tests drive the same one
package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hackclub/ari-webhooks/internal/autoreject"
	"github.com/hackclub/ari-webhooks/internal/config"
	"github.com/hackclub/ari-webhooks/internal/cryptobox"
	"github.com/hackclub/ari-webhooks/internal/ext"
	"github.com/hackclub/ari-webhooks/internal/httpapi"
	"github.com/hackclub/ari-webhooks/internal/ingest"
	"github.com/hackclub/ari-webhooks/internal/integrations/githost"
	"github.com/hackclub/ari-webhooks/internal/integrations/hackatime"
	"github.com/hackclub/ari-webhooks/internal/integrations/lapse"
	"github.com/hackclub/ari-webhooks/internal/integrations/vmplat"
	"github.com/hackclub/ari-webhooks/internal/jobs"
	"github.com/hackclub/ari-webhooks/internal/outbound"
	"github.com/hackclub/ari-webhooks/internal/pipeline/autocheck"
	"github.com/hackclub/ari-webhooks/internal/pipeline/enrich"
	"github.com/hackclub/ari-webhooks/internal/reingest"
	"github.com/hackclub/ari-webhooks/internal/runner"
	"github.com/hackclub/ari-webhooks/internal/sweep"
)

type App struct {
	Queue    *jobs.Queue
	Outbound *outbound.Worker
	Module   ext.Module
	// exported so a test can allow its local repository
	CaptureGit *githost.Fetcher

	cfg       *config.Config
	github    *githost.GitHub
	pool      *pgxpool.Pool
	ingest    *ingest.Service
	reject    *autoreject.Service
	hackatime *hackatime.Client
}

func New(cfg *config.Config, pool *pgxpool.Pool, codec *cryptobox.Codec) *App {
	queue := jobs.NewQueue(pool, cfg.WorkerId)
	ingestService := &ingest.Service{
		Pool:  pool,
		Codec: codec,
		// enrich commits atomically with the ship (see ingest.Service.EnqueueEnrich):
		// an accepted ship can never exist without the job that drives it forward.
		EnqueueEnrich: func(ctx context.Context, tx pgx.Tx, submissionId string) error {
			return jobs.EnqueueTx(ctx, tx, "enrich", submissionId, time.Now())
		},
		// autocheck/screen are auxiliary: a lost enqueue degrades a check but never
		// wedges the ship, so they stay best-effort post-commit followups.
		Followups: func(ctx context.Context, submissionId string) {
			now := time.Now()
			for kind, runAt := range map[string]time.Time{
				"autocheck": now.Add(5 * time.Second),
				"screen":    now,
			} {
				if err := queue.Enqueue(ctx, kind, submissionId, runAt); err != nil {
					slog.Error("followup enqueue failed", "kind", kind, "submissionId", submissionId, "err", err)
				}
			}
			queue.Wake()
		},
	}

	worker := outbound.NewWorker(pool, codec, cfg.WorkerId)
	rejectService := &autoreject.Service{Pool: pool, Outbound: worker}
	hackatimeClient := &hackatime.Client{BaseUrl: cfg.HackatimeBaseUrl, AdminKey: cfg.HackatimeAdminKey}
	github := githost.NewGitHub(cfg.GhProxyUrl, cfg.GhProxyApiKey, "")
	captureFetcher := githost.NewFetcher(cfg.GitCloneConcurrency, time.Duration(cfg.GitCloneTimeoutSecs)*time.Second)
	captureFetcher.GitHub = github
	// Built whether or not this process runs jobs: the internal HTTP API needs
	// the module's routes either way.
	module := ext.Load(ext.Deps{
		Pool:       pool,
		Codec:      codec,
		Queue:      queue,
		Outbound:   worker,
		Reject:     rejectService,
		Hackatime:  hackatimeClient,
		CaptureGit: captureFetcher,
	})
	ingestService.OnFraudWithdraw = module.Fraud.OnWithdrawn
	rejectService.OnFraudDecided = module.Fraud.OnDecided
	ingestService.OnDisallowedProject = func(ctx context.Context, submissionId string) {
		rejectService.RequestChanges(ctx, autoreject.Input{
			SubmissionId: submissionId,
			Reason:       autoreject.DisallowedHackatimeProject,
		})
	}

	return &App{
		Queue:      queue,
		Outbound:   worker,
		Module:     module,
		CaptureGit: captureFetcher,
		github:     github,
		cfg:        cfg,
		pool:       pool,
		ingest:     ingestService,
		reject:     rejectService,
		hackatime:  hackatimeClient,
	}
}

func (a *App) RegisterJobs() *reingest.Service {
	pool, queue, module, worker := a.pool, a.Queue, a.Module, a.Outbound
	autocheckPipeline := &autocheck.Pipeline{Pool: pool, Reject: a.reject}
	enrichPipeline := &enrich.Pipeline{
		Pool:       pool,
		Reject:     a.reject,
		Fraud:      module.Fraud,
		FlagChecks: module.FlagChecks,
		Evidence:   module.Evidence,
		Git:        a.CaptureGit,
		Hackatime:  a.hackatime,
		Lapse:      &lapse.Client{BaseUrl: a.cfg.LapseBaseUrl, ApiKey: a.cfg.LapseApiKey},
	}
	queue.Register("enrich", enrichPipeline.Handle)
	queue.Register("reenrich", enrichPipeline.HandleReenrich)
	queue.Register("autocheck", autocheckPipeline.Handle)
	queue.Register("screen", module.Screener.Screen)
	for kind, handler := range module.Jobs {
		queue.Register(kind, handler)
	}
	reingestService := &reingest.Service{
		Pool:  pool,
		Queue: queue,
		Capture: func(ctx context.Context, submissionId string) (bool, error) {
			result, err := enrichPipeline.Recapture(ctx, submissionId)
			return result.OK, err
		},
		OnFraudReset: module.Fraud.OnReingested,
		OnDecisionInvalidated: func(ctx context.Context, in reingest.DecisionInvalidation) error {
			action := "returned it to review"
			if in.TargetStatus == "reverted" {
				action = "invalidated it because another revision of the project is already open"
			}
			audit := fmt.Sprintf(
				"Ari reprocessed this ship with evidence algorithm version %d and %s because its verified hours changed (previous status: %s).",
				ingest.CurrentEvidenceVersion, action, in.PriorStatus)
			return worker.DispatchReviewResult(ctx, outbound.DispatchInput{
				DeliveryId:   in.ActivityEventId,
				Event:        in.Event,
				ProgramId:    in.ProgramId,
				SubmissionId: in.SubmissionId,
				ReviewerId:   autoreject.SystemUserId,
				Note:         "",
				AuditNote:    &audit,
			})
		},
		AfterReingest: func(ctx context.Context, submissionId string) {
			now := time.Now()
			for kind, runAt := range map[string]time.Time{
				"autocheck": now.Add(5 * time.Second),
				"screen":    now,
			} {
				if err := queue.Enqueue(ctx, kind, submissionId, runAt); err != nil {
					slog.Error("reingest followup enqueue failed", "kind", kind, "submissionId", submissionId, "err", err)
				}
			}
			queue.Wake()
		},
	}
	queue.Register("reingest", reingestService.Handle)
	return reingestService
}

func (a *App) StartSweeps(ctx context.Context, reingestService *reingest.Service) *runner.Runner {
	pool := a.pool
	sweeps := runner.New(a.cfg.DatabaseUrl)
	vmReaper := &sweep.VmReaper{Pool: pool, Vm: &vmplat.Client{BaseUrl: a.cfg.VmApiBase, Token: a.cfg.VmApiToken}}
	for _, extra := range a.Module.Sweeps {
		sweeps.Every(ctx, extra)
	}
	sweeps.Every(ctx, runner.Sweep{
		Name: "evidenceVersion", Interval: time.Minute, Jitter: 10 * time.Second, LockKey: 7108, Immediate: true,
		Fn: reingestService.Sweep,
	})
	sweeps.Every(ctx, runner.Sweep{
		Name: "claims", Interval: 5 * time.Minute, Jitter: 30 * time.Second, LockKey: 7101,
		Fn: func(ctx context.Context) error { return sweep.ReapStaleClaims(ctx, pool) },
	})
	sweeps.Every(ctx, runner.Sweep{
		Name: "stuck", Interval: 30 * time.Minute, Jitter: 3 * time.Minute, LockKey: 7102,
		Fn: func(ctx context.Context) error { return sweep.ReapStuckSubmissions(ctx, pool, a.reject) },
	})
	sweeps.Every(ctx, runner.Sweep{
		Name: "vms", Interval: 5 * time.Minute, Jitter: 30 * time.Second, LockKey: 7103,
		Fn: func(ctx context.Context) error {
			if err := vmReaper.ReapStale(ctx); err != nil {
				return err
			}
			return vmReaper.ReapTombstones(ctx)
		},
	})
	return sweeps
}

func (a *App) Server() *httpapi.Server {
	// Its own slots, separate from the enrich pipeline's fetcher: a reviewer
	// waiting on a file read must not queue behind capture clones, and the
	// blobless single-file reads are far lighter than captures.
	previewFetcher := githost.NewFetcher(2, time.Duration(a.cfg.GitCloneTimeoutSecs)*time.Second)
	previewFetcher.GitHub = a.github // shared, so both fetchers pass the one limiter the proxy's allowance needs
	return &httpapi.Server{
		Pool:          a.pool,
		Ingest:        a.ingest,
		Jobs:          a.Queue,
		Git:           previewFetcher,
		InternalToken: a.cfg.InternalApiToken,
		Routes:        a.Module.Routes,
	}
}
