package lapse

import (
	"sort"

	"github.com/hackclub/ari-webhooks/internal/integrations/hackatime"
)

// Interval is a half-open wall-clock interval [Start, End) in epoch seconds.
type Interval struct {
	Start float64
	End   float64
}

// MergeIntervals merges into a sorted, disjoint set; drops empty/inverted ones.
func MergeIntervals(intervals []Interval) []Interval {
	var valid []Interval
	for _, i := range intervals {
		if i.End > i.Start {
			valid = append(valid, i)
		}
	}
	sort.Slice(valid, func(a, b int) bool { return valid[a].Start < valid[b].Start })
	var out []Interval
	for _, iv := range valid {
		if len(out) > 0 && iv.Start <= out[len(out)-1].End {
			if iv.End > out[len(out)-1].End {
				out[len(out)-1].End = iv.End
			}
		} else {
			out = append(out, iv)
		}
	}
	return out
}

// CoveredSeconds is the seconds of [start, end) covered by merged (pre-merged, disjoint, sorted).
func CoveredSeconds(start, end float64, merged []Interval) float64 {
	if end <= start {
		return 0
	}
	total := 0.0
	for _, iv := range merged {
		if iv.End <= start {
			continue
		}
		if iv.Start >= end {
			break
		}
		total += min(end, iv.End) - max(start, iv.Start)
	}
	return total
}

// SubtractLapseFromSpans reduces each span's credited duration by the
// wall-clock seconds it shares with a lapse interval for the SAME project, so
// a clip synced into Hackatime is never counted twice. Fully-covered spans are
// dropped; their time is credited under the lapse bucket instead.
func SubtractLapseFromSpans(spans []hackatime.Span, lapseByProject map[string][]Interval) []hackatime.Span {
	if len(lapseByProject) == 0 {
		return spans
	}
	merged := map[string][]Interval{}
	for project, ivs := range lapseByProject {
		merged[project] = MergeIntervals(ivs)
	}
	var out []hackatime.Span
	for _, s := range spans {
		m := merged[s.Project]
		if s.Project == "" || len(m) == 0 {
			out = append(out, s)
			continue
		}
		overlap := CoveredSeconds(s.StartTime, s.EndTime, m)
		if overlap <= 0 {
			out = append(out, s)
			continue
		}
		duration := s.Duration - overlap
		if duration > 0 {
			reduced := s
			reduced.Duration = duration
			out = append(out, reduced)
		}
	}
	return out
}
