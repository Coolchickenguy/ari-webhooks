package filehours

import (
	"reflect"
	"testing"
)

func TestResolveFileHoursTiersAndCasing(t *testing.T) {
	head := map[string]int64{
		"src/App.svelte": 900,
		"README.md":      120,
	}
	history := []string{"src/old/Parser.ts"}
	rows := ResolveFileHours(map[string]float64{
		"src/app.svelte":                    3600, // exact match, casing differs: repo casing must win
		"/Users/mia/dev/proj/README.md":     600,  // suffix match
		"/Users/mia/other/parser.ts":        300,  // base-name match into history only
		"/Users/mia/scratch/experiment.rkt": 45,   // nowhere: keeps only the tail
	}, head, history)

	want := []FileHours{
		{Path: "src/App.svelte", Seconds: 3600, Bytes: 900, Status: "head"},
		{Path: "README.md", Seconds: 600, Bytes: 120, Status: "head"},
		{Path: "src/old/Parser.ts", Seconds: 300, Status: "history"},
		{Path: "mia/scratch/experiment.rkt", Seconds: 45, Status: "none"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %+v\nwant  %+v", rows, want)
	}
}

func TestResolveFileHoursMergesCollaboratorPaths(t *testing.T) {
	head := map[string]int64{"src/main.go": 50}
	rows := ResolveFileHours(map[string]float64{
		"/Users/mia/proj/src/main.go": 1000,
		"C:\\dev\\proj\\src\\main.go": 500,
	}, head, nil)
	if len(rows) != 1 {
		t.Fatalf("two checkouts of one file must merge: %+v", rows)
	}
	if rows[0].Path != "src/main.go" || rows[0].Seconds != 1500 || rows[0].Status != "head" {
		t.Fatalf("merged row: %+v", rows[0])
	}
}

func TestResolveFileHoursPrefersHeadOverHistoryAndLongestSuffix(t *testing.T) {
	head := map[string]int64{
		"app/src/index.ts": 10,
		"src/index.ts":     20,
	}
	// Also present in history; head must win, and the longer suffix must beat the
	// shorter one so the entity lands on the path it actually names.
	rows := ResolveFileHours(map[string]float64{
		"/home/leo/proj/app/src/index.ts": 60,
	}, head, []string{"app/src/index.ts"})
	if len(rows) != 2 || rows[0].Path != "app/src/index.ts" || rows[0].Status != "head" || rows[0].Bytes != 10 {
		t.Fatalf("rows = %+v", rows)
	}
	// The head file the entity did NOT land on still appears, with zero time.
	if rows[1].Path != "src/index.ts" || rows[1].Seconds != 0 || rows[1].Status != "head" || rows[1].Bytes != 20 {
		t.Fatalf("untouched head file: %+v", rows[1])
	}
}

func TestResolveFileHoursIncludesUntouchedHeadFiles(t *testing.T) {
	head := map[string]int64{
		"src/main.go":  50,
		"assets/a.png": 4096,
		"README.md":    120,
	}
	rows := ResolveFileHours(map[string]float64{
		"/home/mia/proj/src/main.go": 300,
	}, head, nil)

	want := []FileHours{
		{Path: "src/main.go", Seconds: 300, Bytes: 50, Status: "head"},
		{Path: "README.md", Seconds: 0, Bytes: 120, Status: "head"},
		{Path: "assets/a.png", Seconds: 0, Bytes: 4096, Status: "head"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %+v\nwant  %+v", rows, want)
	}
}

func TestResolveFileHoursSkipsEmptyAndNonPositive(t *testing.T) {
	rows := ResolveFileHours(map[string]float64{
		"":                0,
		".":               100,
		"/tmp/gone.ts":    0,
		"/tmp/kept.ts":    -5,
		"/tmp/counted.ts": 30,
	}, map[string]int64{}, nil)
	if len(rows) != 1 || rows[0].Path != "tmp/counted.ts" || rows[0].Status != "none" {
		t.Fatalf("rows = %+v", rows)
	}
}
