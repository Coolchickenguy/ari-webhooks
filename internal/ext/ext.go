// the seam between the open core and the optional private module. a build
// without the module gets the no-ops at the bottom of this file
package ext

import (
	"context"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hackclub/ari-webhooks/internal/autoreject"
	"github.com/hackclub/ari-webhooks/internal/cryptobox"
	"github.com/hackclub/ari-webhooks/internal/integrations/githost"
	"github.com/hackclub/ari-webhooks/internal/integrations/hackatime"
	"github.com/hackclub/ari-webhooks/internal/jobs"
	"github.com/hackclub/ari-webhooks/internal/outbound"
	"github.com/hackclub/ari-webhooks/internal/runner"
)

// runs as the "screen" job, queued when a ship is accepted and after a reingest
type Screener interface {
	Screen(ctx context.Context, job jobs.Job) jobs.Outcome
}

type Capture struct {
	SubmissionId string
	// true on the ingest capture only: a recapture of a queued ship must not decide it
	Settle             bool
	HackatimeAttempted bool
	HackatimeHealthy   bool
	HackatimeProjects  []string
	// heartbeat-derived spans only, so end minus start is a real unbroken run
	SpansByMaker map[string][]hackatime.Span
	// nil when no capture carried file identity
	EntitySeconds map[string]float64
	DaySeconds    map[string]float64
	TreeRead      bool
	HeadPaths     []string
	HistoryPaths  []string
	Readme        string
}

// runs after every capture no automated decision already settled
type FlagChecks interface {
	AfterCapture(ctx context.Context, capture Capture) (notes []string, settled bool)
}

// Gather runs before the capture transaction; what it returns is handed back
// to Store inside it. StoreFiles only runs when the file listing was read
type Evidence interface {
	Gather(ctx context.Context, emails []string) (gathered any, notes []string, err error)
	StoreFiles(ctx context.Context, tx pgx.Tx, submissionId string, files []githost.File) (notes []string, err error)
	Store(ctx context.Context, tx pgx.Tx, submissionId string, gathered any) (notes []string, err error)
}

type FraudGateway interface {
	OnEnteredQueue(ctx context.Context, submissionId string)
	OnWithdrawn(ctx context.Context, submissionId string)
	OnDecided(ctx context.Context, submissionId string)
	OnReingested(ctx context.Context, submissionId, activityEventId string)
}

// handlers wrapped in internal require the internal api token
type Routes func(app *fiber.App, internal func(fiber.Handler) fiber.Handler)

type Module struct {
	Screener   Screener
	FlagChecks FlagChecks
	Evidence   Evidence
	Fraud      FraudGateway
	Routes     Routes
	// extra job kinds and sweeps, only started in a process that runs jobs
	Jobs   map[string]jobs.Handler
	Sweeps []runner.Sweep
}

type Deps struct {
	Pool      *pgxpool.Pool
	Codec     *cryptobox.Codec
	Queue     *jobs.Queue
	Outbound  *outbound.Worker
	Reject    *autoreject.Service
	Hackatime *hackatime.Client
	// the evidence capture's fetcher: background work shares its slots
	CaptureGit *githost.Fetcher
}

var builder func(Deps) Module

// call from an init function: the builder is read without a lock
func Register(build func(Deps) Module) {
	builder = build
}

func Registered() bool {
	return builder != nil
}

func Load(deps Deps) Module {
	module := Module{}
	if builder != nil {
		module = builder(deps)
	}
	return fill(module)
}

func Noop() Module {
	return fill(Module{})
}

func fill(module Module) Module {
	if module.Screener == nil {
		module.Screener = noop{}
	}
	if module.FlagChecks == nil {
		module.FlagChecks = noop{}
	}
	if module.Evidence == nil {
		module.Evidence = noop{}
	}
	if module.Fraud == nil {
		module.Fraud = noop{}
	}
	if module.Routes == nil {
		module.Routes = func(*fiber.App, func(fiber.Handler) fiber.Handler) {}
	}
	return module
}

type noop struct{}

func (noop) Screen(context.Context, jobs.Job) jobs.Outcome { return jobs.Done() }

func (noop) AfterCapture(context.Context, Capture) ([]string, bool) { return nil, false }

func (noop) Gather(context.Context, []string) (any, []string, error) { return nil, nil, nil }

func (noop) StoreFiles(context.Context, pgx.Tx, string, []githost.File) ([]string, error) {
	return nil, nil
}

func (noop) Store(context.Context, pgx.Tx, string, any) ([]string, error) { return nil, nil }

func (noop) OnEnteredQueue(context.Context, string) {}

func (noop) OnWithdrawn(context.Context, string) {}

func (noop) OnDecided(context.Context, string) {}

func (noop) OnReingested(context.Context, string, string) {}
