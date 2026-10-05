package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/hackclub/ari-webhooks/internal/config"
	"github.com/hackclub/ari-webhooks/internal/cryptobox"
	"github.com/hackclub/ari-webhooks/internal/db"
	"github.com/hackclub/ari-webhooks/internal/db/migrate"
	"github.com/hackclub/ari-webhooks/internal/ext"
	"github.com/hackclub/ari-webhooks/internal/obs"
)

// Main is the whole process: both entry points call it
func Main() {
	shutdownMonitoring := obs.Setup()
	err := run()
	if err != nil {
		slog.Error("fatal", "err", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	shutdownMonitoring(ctx)
	cancel()
	if err != nil {
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg.DatabaseUrl)
	if err != nil {
		return fmt.Errorf("database connection failed: %w", err)
	}
	defer pool.Close()

	if err := migrate.Run(ctx, pool); err != nil {
		return fmt.Errorf("migrations failed: %w", err)
	}
	slog.Info("migrations applied", "privateModule", ext.Registered())

	codec, err := cryptobox.New(cfg.TokenEncKey)
	if err != nil {
		return err
	}
	core := New(cfg, pool, codec)

	var background sync.WaitGroup
	if cfg.RunJobs {
		sweeps := core.StartSweeps(ctx, core.RegisterJobs())
		background.Add(2)
		go func() {
			defer background.Done()
			core.Queue.Run(ctx, 5*time.Second, cfg.JobWorkers)
		}()
		go func() {
			defer background.Done()
			sweeps.Wait()
		}()
		slog.Info("job workers and sweeps started")
	}

	if cfg.RunOutbox {
		background.Add(2)
		go func() {
			defer background.Done()
			core.Outbound.Run(ctx)
		}()
		go func() {
			defer background.Done()
			core.Outbound.Listen(ctx, cfg.DatabaseUrl)
		}()
		slog.Info("outbox worker started")
	}

	if cfg.ServeHttp {
		if cfg.InternalApiToken == "" {
			slog.Warn("INTERNAL_API_TOKEN is not set; every /internal/* trigger from ari will be rejected with 401")
		}
		server := core.Server().App()
		go func() {
			<-ctx.Done()
			_ = server.Shutdown()
		}()
		slog.Info("listening", "port", cfg.Port)
		if err := server.Listen(fmt.Sprintf(":%d", cfg.Port), fiber.ListenConfig{DisableStartupMessage: true}); err != nil {
			return err
		}
	} else {
		<-ctx.Done()
	}
	background.Wait()
	return nil
}
