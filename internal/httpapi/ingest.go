package httpapi

import (
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/hackclub/ari-webhooks/internal/signature"
)

// The HMAC covers the raw bytes, so the body must be read before any JSON parse.
func (s *Server) ingest(c fiber.Ctx) error {
	startedAt := time.Now()
	programId := c.Params("program")
	rawBody := append([]byte(nil), c.Body()...)
	result := s.Ingest.ProcessIngest(c.Context(), programId,
		rawBody, c.Get(signature.SignatureHeader), c.Get(signature.TimestampHeader), c.Query("shipped_at"))
	level := slog.LevelInfo
	if result.Body["error"] == "bad_signature" {
		level = slog.LevelDebug // unsigned requests arrive in floods and say nothing about the program's traffic
	}
	slog.Log(c.Context(), level, "ingest request completed",
		"programId", programId,
		"httpStatus", result.Status,
		"result", result.Body["status"],
		"errorCode", result.Body["error"],
		"invalidField", result.Body["field"],
		"submissionId", result.Body["id"],
		"payloadBytes", len(rawBody),
		"signaturePresent", c.Get(signature.SignatureHeader) != "",
		"timestampPresent", c.Get(signature.TimestampHeader) != "",
		"durationMs", time.Since(startedAt).Milliseconds())
	return c.Status(result.Status).JSON(result.Body)
}

func (s *Server) withdraw(c fiber.Ctx) error {
	result := s.Ingest.ProcessWithdraw(c.Context(), c.Params("program"),
		append([]byte(nil), c.Body()...), c.Get(signature.SignatureHeader), c.Get(signature.TimestampHeader))
	return c.Status(result.Status).JSON(result.Body)
}

// GET has no body for the HMAC to cover, so status auth is the webhook secret
// itself as a bearer token instead of a signature over it.
func (s *Server) shipStatus(c fiber.Ctx) error {
	result := s.Ingest.ProcessStatus(c.Context(), c.Params("program"),
		c.Get("Authorization"), c.Query("id"), c.Query("external_id"))
	return c.Status(result.Status).JSON(result.Body)
}
