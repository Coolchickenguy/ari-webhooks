package lapse

import (
	"testing"

	"github.com/hackclub/ari-webhooks/internal/integrations/hackatime"
)

func TestMergeIntervals(t *testing.T) {
	merged := MergeIntervals([]Interval{
		{Start: 100, End: 200},
		{Start: 150, End: 250},
		{Start: 300, End: 400},
		{Start: 400, End: 450}, // adjacent: merges (start <= last end)
		{Start: 500, End: 500}, // empty: dropped
		{Start: 700, End: 600}, // inverted: dropped
	})
	if len(merged) != 2 {
		t.Fatalf("merged: %+v", merged)
	}
	if merged[0] != (Interval{Start: 100, End: 250}) || merged[1] != (Interval{Start: 300, End: 450}) {
		t.Fatalf("merged: %+v", merged)
	}
}

func TestCoveredSeconds(t *testing.T) {
	merged := []Interval{{Start: 100, End: 200}, {Start: 300, End: 400}}
	if got := CoveredSeconds(150, 350, merged); got != 100 {
		t.Fatalf("covered: %v", got)
	}
	if got := CoveredSeconds(250, 260, merged); got != 0 {
		t.Fatalf("gap coverage: %v", got)
	}
	if got := CoveredSeconds(400, 100, merged); got != 0 {
		t.Fatalf("inverted window: %v", got)
	}
}

func TestSubtractLapseFromSpans(t *testing.T) {
	spans := []hackatime.Span{
		{StartTime: 1000, EndTime: 2000, Duration: 1000, Project: "app"},
		{StartTime: 3000, EndTime: 3500, Duration: 500, Project: "app"},
		{StartTime: 1000, EndTime: 2000, Duration: 1000, Project: "other"},
		{StartTime: 1000, EndTime: 2000, Duration: 1000, Project: ""},
	}
	lapseByProject := map[string][]Interval{
		"app": {{Start: 1500, End: 1800}, {Start: 3000, End: 4000}},
	}
	out := SubtractLapseFromSpans(spans, lapseByProject)
	if len(out) != 3 {
		t.Fatalf("fully covered spans must be dropped: %+v", out)
	}
	if out[0].Duration != 700 { // 1000 - 300 overlap
		t.Fatalf("partial overlap: %+v", out[0])
	}
	if out[1].Project != "other" || out[1].Duration != 1000 {
		t.Fatalf("other project untouched: %+v", out[1])
	}
	if out[2].Project != "" || out[2].Duration != 1000 {
		t.Fatalf("unattributed span never reduced: %+v", out[2])
	}
	if spans[0].Duration != 1000 {
		t.Fatal("inputs must not be mutated")
	}
}
