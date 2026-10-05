package httpapi

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"

	"github.com/hackclub/ari-webhooks/internal/integrations/githost"
)

// RepoReader reads live from a repository's default branch. Satisfied by
// *githost.Fetcher; an interface so handler tests can stub the network away.
type RepoReader interface {
	FetchFile(ctx context.Context, repoUrl, filePath string) githost.FileContent
	FetchRepoContext(ctx context.Context, repoUrl string) githost.RepoContext
}

// internalFileSource serves the review screen's source viewer. The path must be
// one this ship's capture recorded on the default branch (the ariw table is the
// allowlist), and the read happens against the live repository, so a file moved
// or deleted since capture answers with a plain error rather than stale bytes.
func (s *Server) internalFileSource(c fiber.Ctx) error {
	filePath := c.Query("path")
	if filePath == "" || len(filePath) > 4096 || strings.IndexByte(filePath, 0) >= 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"ok": false, "message": "Invalid file path"})
	}
	if s.Git == nil { // fail closed, mirroring the blank-token stance
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"ok": false, "message": "File preview is not available on this deployment"})
	}

	// Bounded by the request (unlike the write triggers' detached context): once
	// ari's proxy gives up there is nobody left to read the bytes.
	ctx, cancel := context.WithTimeout(c.Context(), 90*time.Second)
	defer cancel()

	submissionId := c.Params("submission")
	var repoUrl string
	var bytes *int64
	err := s.Pool.QueryRow(ctx, `
		select sub."repoUrl", f.bytes
		from ariw."submissionFileHours" f
		join public."Submission" sub on sub.id = f."submissionId"
		where f."submissionId" = $1 and f.path = $2 and f.status = 'head'`,
		submissionId, filePath).Scan(&repoUrl, &bytes)
	if err == pgx.ErrNoRows {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"ok": false, "message": "This file is not part of the ship's repository capture"})
	}
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"ok": false, "message": "Could not look up the file"})
	}
	if bytes != nil && *bytes > githost.MaxPreviewBytes {
		return c.JSON(fiber.Map{"ok": false, "message": "This file is too large to preview"})
	}

	res := s.Git.FetchFile(ctx, repoUrl, filePath)
	if !res.OK {
		slog.Warn("file source read failed", "submissionId", submissionId, "path", filePath, "err", res.Error)
		return c.JSON(fiber.Map{"ok": false, "message": "Could not read this file from the repository. It may have moved or been deleted since the last capture, or the repository may be unreachable."})
	}
	if res.Binary {
		return c.JSON(fiber.Map{"ok": false, "message": "This is a binary file, there is no source to preview"})
	}
	return c.JSON(fiber.Map{"ok": true, "content": res.Content, "truncated": res.Truncated})
}
