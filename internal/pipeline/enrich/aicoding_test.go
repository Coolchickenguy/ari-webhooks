package enrich_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hackclub/ari-webhooks/internal/integrations/hackatime"
)

// End to end: a hand-written run and an "ai coding" run through the real capture.
// The AI run is credited at one third, the hand-written one in full.
func TestEnrichCreditsAiCodingAtOneThird(t *testing.T) {
	repoUrl := serveRepoWith(t, map[string]string{"src/game.ts": filler("game")})
	f := setupEnrich(t, repoUrl, []string{"snake"})
	ctx := context.Background()

	fourDaysAgo := float64(time.Now().Add(-4 * 24 * time.Hour).Unix())
	// 10 hand-written beats: 9 gaps x 120s = 18m, session 0 so no re-entry credit.
	// 40 AI beats far later: 39 gaps x 120s = 78m, plus the 120s re-entry credit that
	// follows its AI opener, 80m of AI time in all. Undiscounted total 98m.
	beats := append(
		runOn("/home/mia/snake/src/game.ts", "", fourDaysAgo, 10),
		runOn("/home/mia/snake/src/game.ts", "ai coding", fourDaysAgo+50000, 40)...,
	)
	f.pipeline.Hackatime = &hackatime.Client{BaseUrl: serveHackatime(t, beats), AdminKey: "test"}
	if _, err := f.pool.Exec(ctx,
		`update "Maker" set "hackatimeUserId" = 'ht-1' where email = 'mia@example.com'`); err != nil {
		t.Fatal(err)
	}

	result, err := f.pipeline.Enrich(ctx, f.subId)
	if err != nil || !result.OK {
		t.Fatalf("capture must be healthy: ok=%v err=%v notes=%v", result.OK, err, result.Notes)
	}

	// 18m in full + 80m at one third (26.7m) = 44.7m credited (rounds to 45),
	// 53.3m removed (rounds to 53).
	var minutes, discounted int
	if err := f.pool.QueryRow(ctx,
		`select "hackatimeMinutes", "aiDiscountedMinutes" from "HoursBreakdown" where "submissionId" = $1`,
		f.subId).Scan(&minutes, &discounted); err != nil {
		t.Fatal(err)
	}
	if minutes != 45 {
		t.Fatalf("hackatimeMinutes = %d, want 45: 18m hand-written plus 80m of AI time at one third", minutes)
	}
	if discounted != 53 {
		t.Fatalf("aiDiscountedMinutes = %d, want the 53 removed minutes visible to the review screen", discounted)
	}

	// The removed 53m must be visible in the capture notes, not silent.
	found := false
	for _, note := range result.Notes {
		if note == "ai_coding_discounted_53m" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the discount must leave a note: %v", result.Notes)
	}
}

// The same run without the category: nothing is discounted, so trackers that never
// send a category stay at full credit.
func TestEnrichLeavesUncategorizedTimeAlone(t *testing.T) {
	repoUrl := serveRepoWith(t, map[string]string{"src/game.ts": filler("game")})
	f := setupEnrich(t, repoUrl, []string{"snake"})
	ctx := context.Background()

	fourDaysAgo := float64(time.Now().Add(-4 * 24 * time.Hour).Unix())
	f.pipeline.Hackatime = &hackatime.Client{
		BaseUrl:  serveHackatime(t, runOn("/home/mia/snake/src/game.ts", "", fourDaysAgo, 10)),
		AdminKey: "test",
	}
	if _, err := f.pool.Exec(ctx,
		`update "Maker" set "hackatimeUserId" = 'ht-1' where email = 'mia@example.com'`); err != nil {
		t.Fatal(err)
	}

	result, err := f.pipeline.Enrich(ctx, f.subId)
	if err != nil || !result.OK {
		t.Fatalf("capture must be healthy: ok=%v err=%v notes=%v", result.OK, err, result.Notes)
	}
	if result.HackatimeMinutes != 18 {
		t.Fatalf("hackatimeMinutes = %d, want the full 18m", result.HackatimeMinutes)
	}
	var discounted int
	if err := f.pool.QueryRow(ctx,
		`select "aiDiscountedMinutes" from "HoursBreakdown" where "submissionId" = $1`, f.subId).Scan(&discounted); err != nil {
		t.Fatal(err)
	}
	if discounted != 0 {
		t.Fatalf("aiDiscountedMinutes = %d, want 0 with nothing discounted", discounted)
	}
	for _, note := range result.Notes {
		if strings.HasPrefix(note, "ai_coding_discounted") {
			t.Fatalf("nothing to discount: %v", result.Notes)
		}
	}
}
