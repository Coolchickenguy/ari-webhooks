package enrich_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hackclub/ari-webhooks/internal/pipeline/enrich"
	"github.com/hackclub/ari-webhooks/internal/pipeline/enrich/enrichtest"
)

type enrichFixture struct {
	pool     *pgxpool.Pool
	pipeline *enrich.Pipeline
	subId    string
	shared   enrichtest.Fixture
}

type stubBeat = enrichtest.StubBeat

func setupEnrich(t *testing.T, repoUrl string, hackatimeProjects []string) enrichFixture {
	t.Helper()
	shared := enrichtest.Setup(t, repoUrl, hackatimeProjects)
	return enrichFixture{pool: shared.Pool, pipeline: shared.Pipeline, subId: shared.SubId, shared: shared}
}

func (f enrichFixture) snapshot(t *testing.T) (version, commits int, status string) {
	t.Helper()
	return f.shared.Snapshot(t)
}

func serveRepo(t *testing.T) string {
	return enrichtest.ServeRepo(t)
}

func serveRepoWith(t *testing.T, extra map[string]string) string {
	return enrichtest.ServeRepoWith(t, extra)
}

func serveHackatime(t *testing.T, beats []stubBeat) string {
	return enrichtest.ServeHackatime(t, beats)
}

func runOn(entity, category string, startTs float64, beats int) []stubBeat {
	return enrichtest.RunOn(entity, category, startTs, beats)
}

func filler(marker string) string {
	return enrichtest.Filler(marker)
}
