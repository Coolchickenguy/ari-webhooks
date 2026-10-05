package ingest

import (
	"strings"
	"testing"
)

func base() map[string]string {
	return map[string]string{
		"external_id":   `"p1"`,
		"maker":         `{"email": "a@x.com", "name": "A", "slack_id": "U1"}`,
		"title":         `"T"`,
		"description":   `"D"`,
		"repo_url":      `"https://github.com/a/b"`,
		"demo_url":      `"https://demo.x"`,
		"thumbnail_url": `"https://img.x/t.png"`,
		"journals":      `[{"at": "2026-06-01", "minutes": 5, "text": "x"}]`,
	}
}

func render(m map[string]string) []byte {
	var parts []string
	for k, v := range m {
		parts = append(parts, `"`+k+`": `+v)
	}
	return []byte("{" + strings.Join(parts, ", ") + "}")
}

// Every 422 field string is an external API contract; these must match webhook.ts byte for byte.
func TestValidateFieldStrings(t *testing.T) {
	mutate := func(fn func(m map[string]string)) []byte {
		m := base()
		fn(m)
		return render(m)
	}
	cases := []struct {
		name  string
		body  []byte
		field string
	}{
		{"not json", []byte("nope"), "body"},
		{"not object", []byte(`[1]`), "body"},
		{"missing external_id", mutate(func(m map[string]string) { delete(m, "external_id") }), "external_id"},
		{"blank external_id", mutate(func(m map[string]string) { m["external_id"] = `"  "` }), "external_id"},
		{"missing maker", mutate(func(m map[string]string) { delete(m, "maker") }), "maker.email"},
		{"maker email missing", mutate(func(m map[string]string) { m["maker"] = `{"name": "A", "slack_id": "U1"}` }), "maker.email"},
		{"missing title", mutate(func(m map[string]string) { delete(m, "title") }), "title"},
		{"missing description", mutate(func(m map[string]string) { delete(m, "description") }), "description"},
		{"bad repo scheme", mutate(func(m map[string]string) { m["repo_url"] = `"javascript:alert(1)"` }), "repo_url"},
		{"maker name missing", mutate(func(m map[string]string) { m["maker"] = `{"email": "a@x.com", "slack_id": "U1"}` }), "maker.name"},
		{"maker slack missing", mutate(func(m map[string]string) { m["maker"] = `{"email": "a@x.com", "name": "A"}` }), "maker.slack_id"},
		{"track non-string", mutate(func(m map[string]string) { m["track"] = `3` }), "track"},
		{"track null", mutate(func(m map[string]string) { m["track"] = `null` }), "track"},
		{"software needs demo", mutate(func(m map[string]string) { delete(m, "demo_url") }), "demo_url"},
		{"demo bad scheme", mutate(func(m map[string]string) { m["demo_url"] = `"ftp://x"` }), "demo_url"},
		{"thumbnail required", mutate(func(m map[string]string) { delete(m, "thumbnail_url") }), "thumbnail_url"},
		{"hackatime_id non-string", mutate(func(m map[string]string) {
			m["maker"] = `{"email": "a@x.com", "name": "A", "slack_id": "U1", "hackatime_id": 5}`
		}), "maker.hackatime_id"},
		{"program hours negative", mutate(func(m map[string]string) {
			m["maker"] = `{"email": "a@x.com", "name": "A", "slack_id": "U1", "program_hours": -1}`
		}), "maker.program_hours"},
		{"program minutes over cap", mutate(func(m map[string]string) {
			m["maker"] = `{"email": "a@x.com", "name": "A", "slack_id": "U1", "program_minutes": 60001}`
		}), "maker.program_hours"},
		{"evidence non-array", mutate(func(m map[string]string) { m["evidence"] = `"commits"` }), "evidence"},
		{"evidence null", mutate(func(m map[string]string) { m["evidence"] = `null` }), "evidence"},
		{"hackatime_projects non-string entry", mutate(func(m map[string]string) { m["hackatime_projects"] = `[3]` }), "hackatime_projects"},
		{"collaborators too many", mutate(func(m map[string]string) {
			m["collaborators"] = `[` + strings.Repeat(`{"email": "a@x.com"},`, 10) + `{"email": "b@x.com"}]`
		}), "collaborators"},
		{"collaborator email missing", mutate(func(m map[string]string) { m["collaborators"] = `[{"name": "B"}]` }), "collaborators.email"},
		{"collaborator dup", mutate(func(m map[string]string) {
			m["collaborators"] = `[{"email": "b@x.com"}, {"email": " B@X.com "}]`
			m["journals"] = `[{"at": "2026-06-01", "minutes": 5, "text": "x", "email": "b@x.com"}]`
		}), "collaborators"},
		{"collaborator program hours bad", mutate(func(m map[string]string) { m["collaborators"] = `[{"email": "b@x.com", "program_hours": "3"}]` }), "collaborators.program_hours"},
		{"collaborator projects non-array", mutate(func(m map[string]string) {
			m["collaborators"] = `[{"email": "b@x.com", "hackatime_projects": "app"}]`
			m["journals"] = `[{"at": "2026-06-01", "minutes": 5, "text": "x", "email": "b@x.com"}]`
		}), "collaborators.hackatime_projects"},
		{"collaborator projects non-string entry", mutate(func(m map[string]string) {
			m["collaborators"] = `[{"email": "b@x.com", "hackatime_projects": [3]}]`
			m["journals"] = `[{"at": "2026-06-01", "minutes": 5, "text": "x", "email": "b@x.com"}]`
		}), "collaborators.hackatime_projects"},
		{"journals too many", mutate(func(m map[string]string) {
			m["journals"] = `[` + strings.Repeat(`{"at": "2026-06-01", "minutes": 5, "text": "x"},`, 200) + `{"at": "2026-06-01", "minutes": 5, "text": "x"}]`
		}), "journals"},
		{"journal bad date", mutate(func(m map[string]string) { m["journals"] = `[{"at": "gibberish", "minutes": 5, "text": "x"}]` }), "journals.at"},
		{"journal minutes over a day", mutate(func(m map[string]string) { m["journals"] = `[{"at": "2026-06-01", "minutes": 1441, "text": "x"}]` }), "journals.minutes"},
		{"journal text missing", mutate(func(m map[string]string) { m["journals"] = `[{"at": "2026-06-01", "minutes": 5}]` }), "journals.text"},
		{"journal email not a collaborator", mutate(func(m map[string]string) {
			m["collaborators"] = `[{"email": "b@x.com", "program_hours": 1}]`
			m["journals"] = `[{"at": "2026-06-01", "minutes": 5, "text": "x", "email": "z@x.com"}]`
		}), "journals.email"},
		{"meta non-object", mutate(func(m map[string]string) { m["meta"] = `[1]` }), "meta"},
		{"meta nested object", mutate(func(m map[string]string) { m["meta"] = `{"k": {"nested": true}}` }), "meta"},
		{"meta nested array in list", mutate(func(m map[string]string) { m["meta"] = `{"k": [[1]]}` }), "meta"},
		{"update_message non-string", mutate(func(m map[string]string) { m["update_message"] = `7` }), "update_message"},
		{"is_update non-bool", mutate(func(m map[string]string) { m["is_update"] = `"yes"` }), "is_update"},
		{"evidence floor", mutate(func(m map[string]string) { delete(m, "journals") }), "hackatime_projects_or_journals_or_program_hours"},
		{"shipped_at malformed", mutate(func(m map[string]string) { m["shipped_at"] = `"13/13/2026"` }), "shipped_at"},
		{"shipped_at too old", mutate(func(m map[string]string) { m["shipped_at"] = `"1999-12-31T00:00:00Z"` }), "shipped_at"},
	}
	for _, tc := range cases {
		v := validate(tc.body)
		if v.ok {
			t.Errorf("%s: expected invalid %q, got ok", tc.name, tc.field)
			continue
		}
		if v.field != tc.field {
			t.Errorf("%s: field %q, want %q", tc.name, v.field, tc.field)
		}
	}
}

func TestValidateDisallowedHackatimeProject(t *testing.T) {
	m := base()
	m["hackatime_projects"] = `["<<LAST_PROJECT>>", " app "]`
	v := validate(render(m))
	if !v.ok {
		t.Fatalf("mixed list should validate, got %q", v.field)
	}
	if len(v.data.HackatimeProjects) != 1 || v.data.HackatimeProjects[0] != "app" {
		t.Fatalf("placeholder must be dropped, real project kept: %+v", v.data.HackatimeProjects)
	}
	if len(v.data.DisallowedHackatimeProjects) != 1 {
		t.Fatalf("dropped placeholder must be reported: %+v", v.data.DisallowedHackatimeProjects)
	}

	m = base()
	delete(m, "journals")
	m["hackatime_projects"] = `[" <<last_project>> "]`
	v = validate(render(m))
	if !v.ok {
		t.Fatalf("placeholder-only ship must validate so changes can be requested, got %q", v.field)
	}
	if len(v.data.HackatimeProjects) != 0 || len(v.data.DisallowedHackatimeProjects) != 1 {
		t.Fatalf("case-insensitive placeholder must be dropped and reported: %+v / %+v",
			v.data.HackatimeProjects, v.data.DisallowedHackatimeProjects)
	}
}

func TestValidateCollaboratorProjectUnion(t *testing.T) {
	m := base()
	m["hackatime_projects"] = `["shared"]`
	m["collaborators"] = `[
		{"email": "b@x.com", "hackatime_projects": [" theirs ", "shared", "<<LAST_PROJECT>>"]},
		{"email": "c@x.com", "hackatime_projects": ["theirs", "another"]}
	]`
	m["journals"] = `[{"at": "2026-06-01", "minutes": 5, "text": "x", "email": "b@x.com"}]`
	v := validate(render(m))
	if !v.ok {
		t.Fatalf("collaborator projects should validate, got %q", v.field)
	}
	want := []string{"shared", "theirs", "another"}
	if len(v.data.HackatimeProjects) != len(want) {
		t.Fatalf("union must dedupe preserving order: %+v", v.data.HackatimeProjects)
	}
	for i, p := range want {
		if v.data.HackatimeProjects[i] != p {
			t.Fatalf("union order: got %+v, want %+v", v.data.HackatimeProjects, want)
		}
	}
	if len(v.data.DisallowedHackatimeProjects) != 1 {
		t.Fatalf("collaborator placeholder must be reported: %+v", v.data.DisallowedHackatimeProjects)
	}

	// A ship whose only projects come from collaborators clears the evidence floor.
	m = base()
	delete(m, "journals")
	m["collaborators"] = `[{"email": "b@x.com", "hackatime_projects": ["solo-proj"]}]`
	v = validate(render(m))
	if !v.ok {
		t.Fatalf("collaborator-declared projects must satisfy the evidence floor, got %q", v.field)
	}
	if len(v.data.HackatimeProjects) != 1 || v.data.HackatimeProjects[0] != "solo-proj" {
		t.Fatalf("collaborator projects must land in the tracked set: %+v", v.data.HackatimeProjects)
	}
}

func TestValidateAcceptedShapes(t *testing.T) {
	m := base()
	m["track"] = `" HARDWARE "`
	delete(m, "demo_url")
	m["maker"] = `{"email": " A@X.com ", "name": " A ", "slack_id": "U1", "program_minutes": 90.4}`
	delete(m, "journals")
	v := validate(render(m))
	if !v.ok {
		t.Fatalf("hardware ship without demo should validate, got %q", v.field)
	}
	if v.data.Track != "hardware" || v.data.Maker.Email != "a@x.com" || v.data.Maker.ProgramSeconds != 5400 {
		t.Fatalf("normalization: %+v", v.data.Maker)
	}
	if v.data.DemoUrl != nil {
		t.Fatalf("absent demo_url must stay nil, got %q", *v.data.DemoUrl)
	}

	m = base()
	m["update_message"] = `"  polished the ui  "`
	v = validate(render(m))
	if !v.ok || !v.data.IsUpdate || v.data.UpdateMessage == nil || *v.data.UpdateMessage != "polished the ui" {
		t.Fatalf("update message alone implies is_update: %+v", v.data)
	}

	m = base()
	m["meta"] = `{"n": 3, "flag": true, "list": ["a", 2, null], " ": "dropped", "empty": ""}`
	v = validate(render(m))
	if !v.ok {
		t.Fatalf("meta scalars: %q", v.field)
	}
	if v.data.Meta["n"] != "3" || v.data.Meta["flag"] != "true" {
		t.Fatalf("meta scalar coercion must match String(x): %+v", v.data.Meta)
	}
	list := v.data.Meta["list"].([]string)
	if len(list) != 3 || list[0] != "a" || list[1] != "2" || list[2] != "null" {
		t.Fatalf("meta list coercion (null becomes the string null, like JS): %+v", list)
	}
	if _, present := v.data.Meta[" "]; present {
		t.Fatal("blank meta key must be dropped")
	}
	if _, present := v.data.Meta["empty"]; present {
		t.Fatal("empty meta value must be dropped")
	}
}

func TestValidateSecondsPrecedence(t *testing.T) {
	m := base()
	m["maker"] = `{"email": "a@x.com", "name": "A", "slack_id": "U1", "program_seconds": 5429, "program_minutes": 10, "program_hours": 3}`
	m["journals"] = `[
		{"at": "2026-06-01", "seconds": 1830, "minutes": 5, "hours": 2, "text": "x"},
		{"at": "2026-06-02", "minutes": 12.4, "hours": 2, "text": "y"},
		{"at": "2026-06-03", "hours": 1.5, "text": "z"},
		{"at": "2026-06-04", "seconds": null, "minutes": 7, "text": "w"}
	]`
	v := validate(render(m))
	if !v.ok {
		t.Fatalf("seconds ship should validate, got %q", v.field)
	}
	if v.data.Maker.ProgramSeconds != 5429 {
		t.Fatalf("program_seconds must win: %d", v.data.Maker.ProgramSeconds)
	}
	want := []int{1830, 720, 5400, 420}
	for i, j := range v.data.Journals {
		if j.Seconds != want[i] {
			t.Fatalf("journal %d = %ds, want %ds", i, j.Seconds, want[i])
		}
	}

	m = base()
	m["maker"] = `{"email": "a@x.com", "name": "A", "slack_id": "U1", "program_minutes": 10, "program_hours": 3}`
	if v = validate(render(m)); !v.ok || v.data.Maker.ProgramSeconds != 600 {
		t.Fatalf("program_minutes must beat program_hours and store minutes * 60: %+v", v.data.Maker)
	}

	rejected := []struct {
		name, key, value, field string
	}{
		{"fractional program seconds", "maker", `{"email": "a@x.com", "name": "A", "slack_id": "U1", "program_seconds": 10.5}`, "maker.program_hours"},
		{"negative program seconds", "maker", `{"email": "a@x.com", "name": "A", "slack_id": "U1", "program_seconds": -1}`, "maker.program_hours"},
		{"program seconds over the bound", "maker", `{"email": "a@x.com", "name": "A", "slack_id": "U1", "program_seconds": 3600001}`, "maker.program_hours"},
		{"text program seconds", "maker", `{"email": "a@x.com", "name": "A", "slack_id": "U1", "program_seconds": "60", "program_minutes": 1}`, "maker.program_hours"},
		{"fractional journal seconds", "journals", `[{"at": "2026-06-01", "seconds": 0.5, "minutes": 5, "text": "x"}]`, "journals.minutes"},
		{"negative journal seconds", "journals", `[{"at": "2026-06-01", "seconds": -5, "text": "x"}]`, "journals.minutes"},
		{"journal seconds over a day", "journals", `[{"at": "2026-06-01", "seconds": 86401, "text": "x"}]`, "journals.minutes"},
	}
	for _, c := range rejected {
		m := base()
		m[c.key] = c.value
		if v := validate(render(m)); v.ok || v.field != c.field {
			t.Fatalf("%s: ok=%v field=%q, want %q", c.name, v.ok, v.field, c.field)
		}
	}

	m = base()
	m["maker"] = `{"email": "a@x.com", "name": "A", "slack_id": "U1", "program_seconds": 3600000}`
	m["journals"] = `[{"at": "2026-06-01", "seconds": 86400, "text": "x"}]`
	if v = validate(render(m)); !v.ok {
		t.Fatalf("the bounds themselves are accepted, got %q", v.field)
	}
}
