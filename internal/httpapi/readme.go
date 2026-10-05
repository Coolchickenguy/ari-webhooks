package httpapi

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"
)

// internalReadme reads the ship repository's root README, live, for the review
// screen's README tab. ari renders the markdown itself.
func (s *Server) internalReadme(c fiber.Ctx) error {
	if s.Git == nil { // fail closed, mirroring filesource
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"ok": false, "message": "README preview is not available on this deployment"})
	}
	// Bounded by the request, like filesource: once ari's proxy gives up there
	// is nobody left to read the bytes.
	ctx, cancel := context.WithTimeout(c.Context(), 90*time.Second)
	defer cancel()

	submissionId := c.Params("submission")
	var repoUrl string
	err := s.Pool.QueryRow(ctx, `select "repoUrl" from "Submission" where id = $1`, submissionId).Scan(&repoUrl)
	if err == pgx.ErrNoRows {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"ok": false, "message": "Submission not found"})
	}
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"ok": false, "message": "Could not look up the submission"})
	}
	if strings.TrimSpace(repoUrl) == "" {
		return c.JSON(fiber.Map{"ok": false, "message": "This ship has no repository link"})
	}

	repo := s.Git.FetchRepoContext(ctx, repoUrl)
	if !repo.OK {
		slog.Warn("readme read failed", "submissionId", submissionId, "err", repo.Error)
		return c.JSON(fiber.Map{"ok": false, "message": "Could not read the repository. It may be private, unreachable, or not a git repository."})
	}
	if repo.ReadmeName == "" {
		return c.JSON(fiber.Map{"ok": false, "message": "The repository has no README."})
	}
	if repo.Error != "" {
		slog.Warn("readme read failed", "submissionId", submissionId, "err", repo.Error)
		return c.JSON(fiber.Map{"ok": false, "message": "Could not read the README from the repository. Try again in a moment."})
	}
	return c.JSON(fiber.Map{"ok": true, "readme": repo.Readme, "truncated": repo.ReadmeTruncated})
}
