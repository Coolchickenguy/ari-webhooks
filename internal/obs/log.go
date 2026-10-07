package obs

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/getsentry/sentry-go"
	sentryslog "github.com/getsentry/sentry-go/slog"
	"github.com/joho/godotenv"
)

func Setup() func(context.Context) {
	_ = godotenv.Load()
	stdout := slog.NewJSONHandler(os.Stdout, nil)
	if err := sentry.Init(sentry.ClientOptions{
		EnableTracing:    true,
		TracesSampleRate: 1,
		DataCollection: &sentry.DataCollection{
			HTTPBodies:  []sentry.BodyType{},
			QueryParams: &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff},
			HTTPHeaders: &sentry.HeaderCollectionConfig{
				Request: &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff}, // the ingest signature and the internal bearer token travel in request headers
			},
		},
		BeforeSend:            sanitizeEvent,
		BeforeSendTransaction: sanitizeEvent,
	}); err != nil {
		slog.SetDefault(slog.New(stdout))
		slog.Error("Sentry could not start", "err", err)
		return func(context.Context) {}
	}
	slog.SetDefault(slog.New(slog.NewMultiHandler(
		stdout,
		sentryslog.Option{LogLevel: []slog.Level{slog.LevelInfo, slog.LevelWarn, slog.LevelError}}.NewSentryHandler(context.Background()),
	)))
	sentry.NewMeter(context.Background()).Count("application.started", 1)
	return func(ctx context.Context) {
		sentry.FlushWithContext(ctx)
	}
}

func sanitizeEvent(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	if event.Request == nil {
		return event
	}
	event.Request.Data = ""        // webhook payloads can contain private submission data
	event.Request.QueryString = "" // query parameters can carry upstream API credentials
	event.Request.Headers = nil    // the ingest signature and the internal bearer token travel in headers, whatever the collection setting
	for _, prefix := range tokenPaths {
		if before, _, found := strings.Cut(event.Request.URL, prefix); found {
			event.Request.URL = before + prefix + ":token" // whatever follows the prefix is a secret that must not reach the error tracker
		}
	}
	return event
}

var tokenPaths []string

// reported urls drop whatever follows the prefix. call from an init function
func HideTokenAfter(prefix string) {
	tokenPaths = append(tokenPaths, prefix)
}

// for tests outside this package
func SanitizeEvent(event *sentry.Event) *sentry.Event {
	return sanitizeEvent(event, nil)
}
