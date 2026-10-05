package httpapi

import (
	"context"
	"strings"
	"testing"

	"github.com/hackclub/ari-webhooks/internal/ids"
	"github.com/hackclub/ari-webhooks/internal/integrations/githost"
)

func TestInternalReadme(t *testing.T) {
	stub := &stubRepoReader{context: githost.RepoContext{OK: true, ReadmeName: "README.md", Readme: "# Hello\n"}}
	pool, app, subId := shipFixture(t, func(s *Server) { s.Git = stub })

	code, body := callInternal(t, app, "GET", "/internal/readme/"+subId, "sekrit", "")
	if code != 200 || body["ok"] != true || body["readme"] != "# Hello\n" {
		t.Fatalf("readme read: %d %v", code, body)
	}

	stub.context = githost.RepoContext{OK: true}
	if _, body = callInternal(t, app, "GET", "/internal/readme/"+subId, "sekrit", ""); body["ok"] != false {
		t.Fatalf("a repo without a README answers a plain no: %v", body)
	}

	stub.context = githost.RepoContext{Error: "clone_failed: fatal: repository not found"}
	if _, body = callInternal(t, app, "GET", "/internal/readme/"+subId, "sekrit", ""); body["ok"] != false {
		t.Fatalf("an unreadable repo answers a plain no: %v", body)
	}
	if msg, _ := body["message"].(string); strings.Contains(msg, "fatal") {
		t.Fatalf("raw git errors stay in the logs, not the reviewer's screen: %q", msg)
	}

	if _, err := pool.Exec(context.Background(), `update "Submission" set "repoUrl" = '' where id = $1`, subId); err != nil {
		t.Fatal(err)
	}
	if _, body = callInternal(t, app, "GET", "/internal/readme/"+subId, "sekrit", ""); body["ok"] != false {
		t.Fatalf("a ship without a repo link answers a plain no: %v", body)
	}

	if code, _ = callInternal(t, app, "GET", "/internal/readme/"+ids.ShipId(), "sekrit", ""); code != 404 {
		t.Fatalf("unknown ship: %d", code)
	}
}

func TestInternalReadmeFailsClosedWithoutFetcher(t *testing.T) {
	_, app, subId := shipFixture(t, nil)
	if code, _ := callInternal(t, app, "GET", "/internal/readme/"+subId, "sekrit", ""); code != 503 {
		t.Fatalf("no fetcher wired: %d, want 503", code)
	}
}
