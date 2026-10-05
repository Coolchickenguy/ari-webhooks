package httpapi

import (
	"context"
	"errors"
	"time"

	sentryfiber "github.com/getsentry/sentry-go/fiberv3"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hackclub/ari-webhooks/internal/ext"
	"github.com/hackclub/ari-webhooks/internal/ingest"
	"github.com/hackclub/ari-webhooks/internal/jobs"
)

type Server struct {
	Pool          *pgxpool.Pool
	Ingest        *ingest.Service
	Jobs          *jobs.Queue
	Git           RepoReader
	InternalToken string
	Routes        ext.Routes
}

func (s *Server) App() *fiber.App {
	app := fiber.New(fiber.Config{
		BodyLimit: 25 << 20, // 200 journals x 50KB markdown can exceed the 4MB default
	})
	app.Get("/healthz", s.health)
	app.Use(recover.New())
	app.Use(sentryfiber.New(sentryfiber.Options{Repanic: true}))
	app.Use(captureServerError)
	app.Post("/api/ingest/:program", s.ingest)
	app.Post("/api/ingest/:program/withdraw", s.withdraw)
	app.Get("/api/ingest/:program/status", s.shipStatus)
	app.Post("/internal/reenrich/:submission", s.internalAuth(s.internalReenrich))
	app.Post("/internal/reenrich-program/:program", s.internalAuth(s.internalReenrichProgram))
	app.Get("/internal/filesource/:submission", s.internalAuth(s.internalFileSource))
	app.Get("/internal/readme/:submission", s.internalAuth(s.internalReadme))
	if s.Routes != nil {
		s.Routes(app, s.internalAuth)
	}
	app.All("/internal/*", s.internalAuth(notAvailable)) // after every real route: an internal call nothing in this build answers
	return app
}

func notAvailable(c fiber.Ctx) error {
	return c.Status(fiber.StatusNotImplemented).JSON(fiber.Map{"ok": false, "message": "This feature is not available on this deployment"})
}

func captureServerError(c fiber.Ctx) error {
	err := c.Next()
	if err == nil {
		return nil
	}
	status := fiber.StatusInternalServerError
	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		status = fiberErr.Code
	}
	if status >= fiber.StatusInternalServerError {
		if hub := sentryfiber.GetHubFromContext(c); hub != nil {
			hub.CaptureException(err)
		}
	}
	return err
}

func (s *Server) health(c fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(c.Context(), 2*time.Second)
	defer cancel()
	if err := s.Pool.Ping(ctx); err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"ok": false})
	}
	return c.JSON(fiber.Map{"ok": true})
}
