// fixtures the capture tests share: a throwaway database with one processing
// ship, a real git repository served over http, a stand-in for the hackatime api
package enrichtest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hackclub/ari-webhooks/internal/autoreject"
	"github.com/hackclub/ari-webhooks/internal/cryptobox"
	"github.com/hackclub/ari-webhooks/internal/ext"
	"github.com/hackclub/ari-webhooks/internal/ids"
	"github.com/hackclub/ari-webhooks/internal/integrations/githost"
	"github.com/hackclub/ari-webhooks/internal/integrations/hackatime"
	"github.com/hackclub/ari-webhooks/internal/integrations/lapse"
	"github.com/hackclub/ari-webhooks/internal/outbound"
	"github.com/hackclub/ari-webhooks/internal/pipeline/enrich"
	"github.com/hackclub/ari-webhooks/internal/testdb"
	"github.com/hackclub/ari-webhooks/internal/testgit"
)

// ServeRepo builds a real repo and serves it over git's smart HTTP protocol.
func ServeRepo(t *testing.T) string {
	return ServeRepoWith(t, nil)
}

// ServeRepoWith adds extra root files (path to contents) on top of ServeRepo's
// single main.go commit.
func ServeRepoWith(t *testing.T, extra map[string]string) string {
	t.Helper()
	src := t.TempDir()
	testgit.Run(t, src, "init", "-q", "-b", "main")
	files := map[string]string{"main.go": "package main\n"}
	for name, contents := range extra {
		files[name] = contents
	}
	for name, contents := range files {
		full := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	testgit.Run(t, src, "add", ".")
	testgit.Run(t, src, "commit", "-q", "-m", "ship it")
	root := t.TempDir()
	bare := filepath.Join(root, "proj.git")
	testgit.Run(t, src, "clone", "-q", "--bare", src, bare)
	return testgit.Serve(t, root).URL + "/proj.git"
}

// ServeGitHubRepo publishes ServeRepoWith's repository as owner1/project1 twice
// over: on a git server that honors --filter, and behind a fake GitHub api proxy. The
// returned client reads that git server's host through the fake.
func ServeGitHubRepo(t *testing.T, extra map[string]string) (repoUrl string, fake *testgit.GitHubFake, github *githost.GitHub) {
	t.Helper()
	src := t.TempDir()
	testgit.Run(t, src, "init", "-q", "-b", "main")
	files := map[string]string{"main.go": "package main\n"}
	for name, contents := range extra {
		files[name] = contents
	}
	for name, contents := range files {
		full := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	testgit.Run(t, src, "add", ".")
	testgit.Run(t, src, "commit", "-q", "-m", "ship it")
	root := t.TempDir()
	bare := filepath.Join(root, "owner1", "project1")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	testgit.Run(t, src, "clone", "-q", "--bare", src, bare)
	testgit.Run(t, bare, "config", "uploadpack.allowFilter", "true")
	testgit.Run(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	srv := testgit.Serve(t, root)
	fake = testgit.ServeGitHub(t, bare, "owner1/project1")
	return srv.URL + "/owner1/project1", fake, githost.NewGitHub(fake.URL, fake.Key, strings.TrimPrefix(srv.URL, "http://"))
}

type Fixture struct {
	Pool     *pgxpool.Pool
	Codec    *cryptobox.Codec
	Outbound *outbound.Worker
	Reject   *autoreject.Service
	Pipeline *enrich.Pipeline
	SubId    string
}

// one processing ship and a pipeline with nothing plugged into the seam
func Setup(t *testing.T, repoUrl string, hackatimeProjects []string) Fixture {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	codec, err := cryptobox.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}

	programId := ids.Cuid()
	makerId := ids.Cuid()
	subId := ids.ShipId()
	if _, err := pool.Exec(ctx, `insert into "Program" (id, name, color) values ($1, 'P', '#000')`, programId); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into "Maker" (id, email, name) values ($1, 'mia@example.com', 'Mia')`, makerId); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into "Submission" (id, "programId", "externalId", "makerId", title, "repoUrl", "claimedHours", status, "hackatimeProjects")
		values ($1, $2, 'ext', $3, 'T', $4, 0, 'processing', $5)`,
		subId, programId, makerId, repoUrl, hackatimeProjects); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into "HoursBreakdown" ("submissionId", "devlogMinutes", "programMinutes") values ($1, 0, 0)`,
		subId); err != nil {
		t.Fatal(err)
	}

	worker := outbound.NewWorker(pool, codec, "test")
	reject := &autoreject.Service{Pool: pool, Outbound: worker}
	git := githost.NewFetcher(2, 0)
	git.AllowPrivateHosts = true // httptest listens on 127.0.0.1
	module := ext.Noop()
	pipeline := &enrich.Pipeline{
		Pool:       pool,
		Reject:     reject,
		Fraud:      module.Fraud,
		FlagChecks: module.FlagChecks,
		Evidence:   module.Evidence,
		Git:        git,
		Hackatime:  &hackatime.Client{}, // no AdminKey: authoritative empty capture
		Lapse:      &lapse.Client{},
	}
	return Fixture{Pool: pool, Codec: codec, Outbound: worker, Reject: reject, Pipeline: pipeline, SubId: subId}
}

func (f Fixture) Snapshot(t *testing.T) (version, commits int, status string) {
	t.Helper()
	if err := f.Pool.QueryRow(context.Background(), `
		select "enrichmentVersion", (select count(*) from "Commit" where "submissionId" = s.id), status::text
		from "Submission" s where id = $1`, f.SubId).Scan(&version, &commits, &status); err != nil {
		t.Fatal(err)
	}
	return version, commits, status
}

// pads a file so it reads as real source
func Filler(marker string) string {
	return "// " + marker + "\n" + strings.Repeat("export const x"+marker+" = 1;\n", 40)
}

type StubBeat struct {
	Time     float64 `json:"time"`
	Project  string  `json:"project"`
	Entity   string  `json:"entity"`
	Kind     string  `json:"type"`
	Category string  `json:"category,omitempty"`
}

// RunOn emits a heartbeat every 120 seconds, the longest gap Hackatime still counts as
// one session, so `beats` beats is (beats-1)*120 seconds credited to entity.
func RunOn(entity, category string, startTs float64, beats int) []StubBeat {
	var out []StubBeat
	for i := range beats {
		out = append(out, StubBeat{
			Time:     startTs + float64(i)*hackatime.HeartbeatCapS,
			Project:  "snake",
			Entity:   entity,
			Kind:     "file",
			Category: category,
		})
	}
	return out
}

// ServeHackatime stands in for the admin API: paged heartbeats, an oracle that reports
// nothing (which the capture treats as agreement rather than a contradiction), and an
// empty timeline.
func ServeHackatime(t *testing.T, beats []StubBeat) string {
	t.Helper()
	sort.Slice(beats, func(a, b int) bool { return beats[a].Time < beats[b].Time })
	mux := http.NewServeMux()
	mux.HandleFunc("/api/admin/v1/user/heartbeats", func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		page := []StubBeat{}
		for i := offset; i < len(beats) && len(page) < limit; i++ {
			page = append(page, beats[i])
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"heartbeats":  page,
			"total_count": len(beats),
			"has_more":    offset+len(page) < len(beats),
		})
	})
	mux.HandleFunc("/api/v1/users/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"projects":[]}`))
	})
	mux.HandleFunc("/api/admin/v1/timeline", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{}`))
	})
	return httptest.NewServer(mux).URL
}
