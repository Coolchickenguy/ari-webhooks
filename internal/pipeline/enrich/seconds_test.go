package enrich_test

import (
	"context"
	"testing"
	"time"

	"github.com/hackclub/ari-webhooks/internal/integrations/hackatime"
	"github.com/hackclub/ari-webhooks/internal/secs"
)

func TestEnrichSplitsWholeSecondsAcrossCollaborators(t *testing.T) {
	repoUrl := serveRepoWith(t, map[string]string{"src/game.ts": filler("game")})
	f := setupEnrich(t, repoUrl, []string{"snake"})
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx, `
		insert into "Maker" (id, email, name, "hackatimeUserId") values ('pal', 'pal@example.com', 'Pal', 'ht-2')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`update "Maker" set "hackatimeUserId" = 'ht-1' where email = 'mia@example.com'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		insert into "SubmissionCollaborator" (id, "submissionId", "makerId")
		select 'collab-a', $1, "makerId" from "Submission" where id = $1
		union all select 'collab-b', $1, 'pal'`, f.subId); err != nil {
		t.Fatal(err)
	}

	// each person: 100s by hand, then a 50s ai gap plus the 120s re-entry at one third
	// (56.67s), 156.67s in all. the ship is 313.33s, so 313, never 157 + 157.
	start := float64(time.Now().Add(-4 * 24 * time.Hour).Unix())
	beat := func(at float64, category string) stubBeat {
		return stubBeat{Time: at, Project: "snake", Entity: "/home/mia/snake/src/game.ts", Kind: "file", Category: category}
	}
	beats := []stubBeat{
		beat(start, ""), beat(start+100, ""),
		beat(start+50000, "ai coding"), beat(start+50050, "ai coding"),
	}
	f.pipeline.Hackatime = &hackatime.Client{BaseUrl: serveHackatime(t, beats), AdminKey: "test"}

	result, err := f.pipeline.Enrich(ctx, f.subId)
	if err != nil || !result.OK {
		t.Fatalf("capture must be healthy: ok=%v err=%v notes=%v", result.OK, err, result.Notes)
	}

	var shipSeconds, shipMinutes, afterSeconds, afterMinutes, aiSeconds, aiMinutes, commitSeconds int
	if err := f.pool.QueryRow(ctx, `
		select "hackatimeSeconds", "hackatimeMinutes", "afterLastCommitSeconds", "afterLastCommitMinutes",
		       "aiDiscountedSeconds", "aiDiscountedMinutes",
		       coalesce((select sum("codingSeconds") from "Commit" where "submissionId" = $1), 0)
		from "HoursBreakdown" where "submissionId" = $1`, f.subId).
		Scan(&shipSeconds, &shipMinutes, &afterSeconds, &afterMinutes, &aiSeconds, &aiMinutes, &commitSeconds); err != nil {
		t.Fatal(err)
	}
	if shipSeconds != 313 || aiSeconds != 227 {
		t.Fatalf("ship seconds = %d, ai discounted = %d, want 313 and 227", shipSeconds, aiSeconds)
	}
	if shipMinutes != secs.LegacyMinutes(shipSeconds) || afterMinutes != secs.LegacyMinutes(afterSeconds) ||
		aiMinutes != secs.LegacyMinutes(aiSeconds) {
		t.Fatalf("minutes must be the legacy view of seconds: %d %d %d", shipMinutes, afterMinutes, aiMinutes)
	}
	if commitSeconds+afterSeconds != shipSeconds {
		t.Fatalf("commits %d + after last commit %d must make up the ship's %d", commitSeconds, afterSeconds, shipSeconds)
	}

	rows, err := f.pool.Query(ctx, `
		select "hackatimeSeconds", "hackatimeMinutes", "afterLastCommitSeconds", "afterLastCommitMinutes",
		       coalesce(("hackatimeProjectSeconds"->>'snake')::int, 0)
		from "SubmissionCollaborator" where "submissionId" = $1 order by id`, f.subId)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var people []int
	total, afterTotal := 0, 0
	for rows.Next() {
		var seconds, minutes, after, afterMins, project int
		if err := rows.Scan(&seconds, &minutes, &after, &afterMins, &project); err != nil {
			t.Fatal(err)
		}
		if minutes != secs.LegacyMinutes(seconds) || afterMins != secs.LegacyMinutes(after) {
			t.Fatalf("collaborator minutes must be the legacy view of %d and %d: %d %d", seconds, after, minutes, afterMins)
		}
		if project != seconds {
			t.Fatalf("project seconds %d must add up to the person's %d", project, seconds)
		}
		people = append(people, seconds)
		total += seconds
		afterTotal += after
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(people) != 2 || people[0] != 157 || people[1] != 156 {
		t.Fatalf("collaborator seconds = %v, want [157 156]", people)
	}
	if total != shipSeconds || afterTotal != afterSeconds {
		t.Fatalf("collaborators sum to %d (after %d), ship has %d (after %d)", total, afterTotal, shipSeconds, afterSeconds)
	}
}
