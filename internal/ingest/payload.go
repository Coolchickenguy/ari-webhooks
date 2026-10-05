package ingest

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type ShipPerson struct {
	Email           string
	Name            string
	SlackId         string
	HackatimeUserId string
	ProgramSeconds  int
}

type ShipJournal struct {
	At       time.Time
	Seconds  int
	Text     string
	Markdown string
	Email    string
}

type ShipPayload struct {
	ExternalId                  string
	Maker                       ShipPerson
	Title                       string
	Description                 string
	RepoUrl                     string
	DemoUrl                     *string
	ThumbnailUrl                *string
	Track                       string
	ShippedAt                   *time.Time
	Evidence                    []string
	HackatimeProjects           []string
	DisallowedHackatimeProjects []string
	Journals                    []ShipJournal
	Meta                        map[string]any
	IsUpdate                    bool
	UpdateMessage               *string
	Collaborators               []ShipPerson
}

var trailingGitSuffix = regexp.MustCompile(`(?i)\.git$`)

// NormalizeRepoUrl mirrors ari's git.ts: strip a trailing .git, then a trailing slash.
func NormalizeRepoUrl(u string) string {
	return strings.TrimSuffix(trailingGitSuffix.ReplaceAllString(u, ""), "/")
}

var httpUrlPattern = regexp.MustCompile(`(?i)^https?://`)

// parseJsDate mirrors `new Date(string)` for the ISO 8601 shapes the ingest
// contract documents: date-time with offset, date-time without offset (local
// time, per the ES spec), and date-only (UTC, per the ES spec).
func parseJsDate(s string) (time.Time, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if d, err := time.Parse(layout, t); err == nil {
			return d, true
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if d, err := time.ParseInLocation(layout, t, time.Local); err == nil {
			return d, true
		}
	}
	if d, err := time.Parse("2006-01-02", t); err == nil {
		return d, true
	}
	return time.Time{}, false
}

// parseShippedAt mirrors webhook.ts parseShippedAt: nil = absent (caller uses
// now()), a time = valid override, ok=false = present but malformed/out of range.
func parseShippedAt(v any) (*time.Time, bool) {
	if v == nil {
		return nil, true
	}
	s, isString := v.(string)
	if !isString {
		return nil, false
	}
	if strings.TrimSpace(s) == "" {
		return nil, true
	}
	d, ok := parseJsDate(s)
	if !ok {
		return nil, false
	}
	if d.After(time.Now().Add(24 * time.Hour)) { // guard format/unit typos: nothing in the far future
		return nil, false
	}
	if d.Before(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) { // nothing absurdly old
		return nil, false
	}
	return &d, true
}

// parseProgramSeconds mirrors webhook.ts: program_seconds beats program_minutes
// beats program_hours, absent = 0, non-finite or out of [0, 60000] minutes =
// invalid (ok=false). Minutes and hours are stored as whole minutes * 60.
func parseProgramSeconds(o map[string]any) (int, bool) {
	if sec, present := o["program_seconds"]; present && sec != nil {
		return wholeSeconds(sec, 3600000) // the same 1000h bound as program_minutes
	}
	min, hasMin := o["program_minutes"]
	hr, hasHr := o["program_hours"]
	if (!hasMin || min == nil) && (!hasHr || hr == nil) {
		return 0, true
	}
	var mins float64
	if f, isNum := min.(float64); isNum {
		mins = f
	} else if f, isNum := hr.(float64); isNum {
		mins = f * 60
	} else {
		return 0, false
	}
	if mins < 0 || mins > 60000 { // 1000h sanity bound on a program-asserted, evidence-free figure
		return 0, false
	}
	return int(jsRound(mins)) * 60, true
}

func wholeSeconds(v any, limit float64) (int, bool) {
	f, isNum := v.(float64)
	if !isNum || f != math.Trunc(f) || f < 0 || f > limit {
		return 0, false
	}
	return int(f), true
}

// jsRound mirrors Math.round: halves round toward positive infinity.
func jsRound(f float64) float64 {
	return math.Floor(f + 0.5)
}

// jsToString mirrors String(x) for the JSON scalar types that reach meta coercion.
func jsToString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return "null"
	}
	return ""
}

func trimmedNonEmpty(v any) (string, bool) {
	s, isString := v.(string)
	if !isString || strings.TrimSpace(s) == "" {
		return "", false
	}
	return s, true
}

func isHttpUrl(v any) bool {
	s, isString := v.(string)
	return isString && httpUrlPattern.MatchString(strings.TrimSpace(s))
}

func sliceRunes(s string, max int) string {
	// JS .slice() counts UTF-16 code units; runes are the closest sane equivalent
	// and only differ on astral pairs at the exact boundary.
	r := []rune(s)
	if len(r) > max {
		return string(r[:max])
	}
	return s
}
