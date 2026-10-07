package httpapi

import (
	"crypto/subtle"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"
)

// The ari-facing internal API: ari triggers work here and reads the outcome
// straight from the shared database.

// internalAuth guards the internal endpoints with the shared INTERNAL_API_TOKEN
// bearer token.
func (s *Server) internalAuth(h fiber.Handler) fiber.Handler {
	return func(c fiber.Ctx) error {
		token := strings.TrimPrefix(c.Get("Authorization"), "Bearer ")
		// Fail closed: no configured token means the internal API is disabled.
		if s.InternalToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.InternalToken)) != 1 {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"message": "Unauthorized"})
		}
		return h(c)
	}
}

// databaseError answers a failed query with a generic message; the detail goes
// to the log only, since table and column names must not leak to the caller.
func databaseError(c fiber.Ctx, what string, err error) error {
	slog.ErrorContext(c.Context(), what, "path", c.Path(), "err", err)
	return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"message": "Database error"})
}

// internalReenrich queues a fresh evidence capture for a ship already in the
// review queue (the review screen's resync button). Async by design: a capture
// can take minutes, so ari polls the submission's
// enrichmentVersion to see the new snapshot land instead of holding the request.
func (s *Server) internalReenrich(c fiber.Ctx) error {
	submissionId := c.Params("submission")
	var status string
	err := s.Pool.QueryRow(c.Context(), `select status::text from "Submission" where id = $1`, submissionId).Scan(&status)
	if err == pgx.ErrNoRows {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"message": "Submission not found"})
	}
	if err != nil {
		return databaseError(c, "reenrich status lookup failed", err)
	}
	// Matches the reenrich handler's own gate; rejecting here gives ari a
	// message it can show instead of a job that silently settles as a no-op.
	if status != "pending" && status != "secondpass" {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"message": "Only a ship waiting for review or second pass can be resynced"})
	}
	if err := s.Jobs.Enqueue(c.Context(), "reenrich", submissionId, time.Now()); err != nil {
		return databaseError(c, "reenrich enqueue failed", err)
	}
	s.Jobs.Wake() // no-op wake when this process runs no workers; the job row is durable either way
	return c.JSON(fiber.Map{"ok": true})
}

// internalReenrichProgram queues a fresh evidence capture for every ship still
// waiting for review in a program (the admin board's bulk re-ingestion). Same
// async contract as internalReenrich, fanned out over the queue; enqueueing is
// idempotent, so a ship whose capture is already queued or running is covered
// by that job rather than getting a duplicate.
func (s *Server) internalReenrichProgram(c fiber.Ctx) error {
	programId := c.Params("program")
	var exists bool
	if err := s.Pool.QueryRow(c.Context(),
		`select exists (select 1 from "Program" where id = $1)`, programId).Scan(&exists); err != nil {
		return databaseError(c, "reenrich program lookup failed", err)
	}
	if !exists {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"message": "Program not found"})
	}
	rows, err := s.Pool.Query(c.Context(),
		`select id from "Submission" where "programId" = $1 and status = 'pending'`, programId)
	if err != nil {
		return databaseError(c, "reenrich program ship listing failed", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return databaseError(c, "reenrich program ship listing failed", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return databaseError(c, "reenrich program ship listing failed", err)
	}
	for _, id := range ids {
		if err := s.Jobs.Enqueue(c.Context(), "reenrich", id, time.Now()); err != nil {
			return databaseError(c, "reenrich program enqueue failed", err)
		}
	}
	if len(ids) > 0 {
		s.Jobs.Wake() // no-op wake when this process runs no workers; the job rows are durable either way
	}
	return c.JSON(fiber.Map{"ok": true, "queued": len(ids)})
}
