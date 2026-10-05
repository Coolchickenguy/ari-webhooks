package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hackclub/ari-webhooks/internal/integrations/githost"
)

type stubFileFetcher struct {
	lastRepoUrl string
	lastPath    string
	result      githost.FileContent
}

func (s *stubFileFetcher) FetchFile(_ context.Context, repoUrl, filePath string) githost.FileContent {
	s.lastRepoUrl = repoUrl
	s.lastPath = filePath
	return s.result
}

func (s *stubFileFetcher) FetchRepoContext(_ context.Context, repoUrl string) githost.RepoContext {
	s.lastRepoUrl = repoUrl
	return githost.RepoContext{}
}

func seedFileHours(t *testing.T, pool *pgxpool.Pool, subId, path, status string, bytes int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		insert into ariw."submissionFileHours" ("submissionId", path, seconds, bytes, status)
		values ($1, $2, 3600, $3, $4)`, subId, path, bytes, status); err != nil {
		t.Fatal(err)
	}
}

func getFileSource(t *testing.T, app *fiber.App, subId, rawQuery, bearer string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", "/internal/filesource/"+subId+rawQuery, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

func TestInternalFileSourceServesARecordedHeadFile(t *testing.T) {
	pool, _, subId := shipFixture(t, nil)
	seedFileHours(t, pool, subId, "src/main.go", "head", 42)
	stub := &stubFileFetcher{result: githost.FileContent{OK: true, Content: "package main\n"}}
	app := (&Server{Pool: pool, Git: stub, InternalToken: "sekrit"}).App()

	code, body := getFileSource(t, app, subId, "?path=src%2Fmain.go", "sekrit")
	if code != 200 || body["ok"] != true || body["content"] != "package main\n" || body["truncated"] != false {
		t.Fatalf("got %d %v", code, body)
	}
	if stub.lastRepoUrl != "https://github.com/a/b" || stub.lastPath != "src/main.go" {
		t.Fatalf("fetch args: %q %q", stub.lastRepoUrl, stub.lastPath)
	}
}

func TestInternalFileSourceOnlyServesTheCapturedAllowlist(t *testing.T) {
	pool, _, subId := shipFixture(t, nil)
	// history/none rows exist but are not viewable; unknown paths 404 too.
	seedFileHours(t, pool, subId, "old/gone.go", "history", 0)
	stub := &stubFileFetcher{result: githost.FileContent{OK: true, Content: "x"}}
	app := (&Server{Pool: pool, Git: stub, InternalToken: "sekrit"}).App()

	if code, _ := getFileSource(t, app, subId, "?path=old%2Fgone.go", "sekrit"); code != 404 {
		t.Fatalf("history row must not be viewable: %d", code)
	}
	if code, _ := getFileSource(t, app, subId, "?path=never%2Fseen.go", "sekrit"); code != 404 {
		t.Fatalf("unknown path must 404: %d", code)
	}
	if stub.lastPath != "" {
		t.Fatalf("no fetch may happen without an allowlisted row: %q", stub.lastPath)
	}
}

func TestInternalFileSourceRejectsBadRequestsAndAuth(t *testing.T) {
	pool, _, subId := shipFixture(t, nil)
	seedFileHours(t, pool, subId, "src/main.go", "head", 42)
	stub := &stubFileFetcher{result: githost.FileContent{OK: true, Content: "x"}}
	app := (&Server{Pool: pool, Git: stub, InternalToken: "sekrit"}).App()

	if code, _ := getFileSource(t, app, subId, "?path=src%2Fmain.go", "wrong"); code != 401 {
		t.Fatalf("wrong token: %d", code)
	}
	if code, _ := getFileSource(t, app, subId, "", "sekrit"); code != 400 {
		t.Fatalf("missing path: %d", code)
	}
	if stub.lastPath != "" {
		t.Fatalf("no fetch may happen on a rejected request: %q", stub.lastPath)
	}
}

func TestInternalFileSourceRefusesOversizedFilesBeforeFetching(t *testing.T) {
	pool, _, subId := shipFixture(t, nil)
	seedFileHours(t, pool, subId, "assets/big.wasm", "head", githost.MaxPreviewBytes+1)
	stub := &stubFileFetcher{result: githost.FileContent{OK: true, Content: "x"}}
	app := (&Server{Pool: pool, Git: stub, InternalToken: "sekrit"}).App()

	code, body := getFileSource(t, app, subId, "?path=assets%2Fbig.wasm", "sekrit")
	if code != 200 || body["ok"] != false {
		t.Fatalf("got %d %v", code, body)
	}
	if stub.lastPath != "" {
		t.Fatalf("an oversized file must be refused without cloning: %q", stub.lastPath)
	}
}

func TestInternalFileSourceReportsReadFailuresPlainly(t *testing.T) {
	pool, _, subId := shipFixture(t, nil)
	seedFileHours(t, pool, subId, "src/main.go", "head", 42)
	stub := &stubFileFetcher{result: githost.FileContent{Error: "read_failed: fatal: path does not exist"}}
	app := (&Server{Pool: pool, Git: stub, InternalToken: "sekrit"}).App()

	code, body := getFileSource(t, app, subId, "?path=src%2Fmain.go", "sekrit")
	if code != 200 || body["ok"] != false {
		t.Fatalf("got %d %v", code, body)
	}
	// The raw git error stays in the logs; the reviewer sees a plain sentence.
	if msg, _ := body["message"].(string); msg == "" || len(msg) < 20 {
		t.Fatalf("message: %v", body["message"])
	}
}
