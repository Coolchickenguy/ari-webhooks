package ingest

import (
	"encoding/json"
	"strings"
)

type validated struct {
	ok    bool
	field string
	data  ShipPayload
}

func invalid(field string) validated {
	return validated{field: field}
}

// The 422 field strings are an external API contract and must stay byte-identical with webhook.ts.
func validate(raw []byte) validated {
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return invalid("body")
	}
	p, isObject := parsed.(map[string]any)
	if !isObject {
		return invalid("body")
	}

	maker, _ := p["maker"].(map[string]any)

	if _, ok := trimmedNonEmpty(p["external_id"]); !ok {
		return invalid("external_id")
	}
	if maker == nil {
		return invalid("maker.email")
	}
	if _, ok := trimmedNonEmpty(maker["email"]); !ok {
		return invalid("maker.email")
	}
	if _, ok := trimmedNonEmpty(p["title"]); !ok {
		return invalid("title")
	}
	if _, ok := trimmedNonEmpty(p["description"]); !ok {
		return invalid("description")
	}
	if !isHttpUrl(p["repo_url"]) { // rendered as a clickable link; a javascript:/data: scheme would be stored link injection
		return invalid("repo_url")
	}
	if _, ok := trimmedNonEmpty(maker["name"]); !ok {
		return invalid("maker.name")
	}
	if _, ok := trimmedNonEmpty(maker["slack_id"]); !ok {
		return invalid("maker.slack_id")
	}
	if track, present := p["track"]; present { // JSON null is rejected too, matching the TS !== undefined check
		if _, isString := track.(string); !isString {
			return invalid("track")
		}
	}
	trackStr, _ := p["track"].(string)
	isHardware := strings.ToLower(strings.TrimSpace(trackStr)) == "hardware"

	_, hasDemo := trimmedNonEmpty(p["demo_url"])
	if !isHardware && !hasDemo { // the live thing reviewers open; hardware has nothing to demo at a URL
		return invalid("demo_url")
	}
	if hasDemo && !isHttpUrl(p["demo_url"]) {
		return invalid("demo_url")
	}
	if !isHttpUrl(p["thumbnail_url"]) {
		return invalid("thumbnail_url")
	}
	if ht, present := maker["hackatime_id"]; present {
		if _, isString := ht.(string); !isString {
			return invalid("maker.hackatime_id")
		}
	}
	makerProgramSeconds, ok := parseProgramSeconds(maker)
	if !ok {
		return invalid("maker.program_hours")
	}
	var evidence []string
	if ev, present := p["evidence"]; present {
		list, isArray := ev.([]any)
		if !isArray {
			return invalid("evidence")
		}
		for _, e := range list {
			s, isString := e.(string)
			if !isString {
				return invalid("evidence")
			}
			evidence = append(evidence, s)
		}
	}
	var hackatimeProjects, disallowedHackatime []string
	if hp, present := p["hackatime_projects"]; present {
		list, isArray := hp.([]any)
		if !isArray {
			return invalid("hackatime_projects")
		}
		for _, e := range list {
			s, isString := e.(string)
			if !isString {
				return invalid("hackatime_projects")
			}
			if t := strings.TrimSpace(s); t != "" {
				if strings.EqualFold(t, "<<LAST_PROJECT>>") { // disallowed for shipping: never stored or credited
					disallowedHackatime = append(disallowedHackatime, t)
					continue
				}
				hackatimeProjects = append(hackatimeProjects, t)
			}
		}
	}

	var collaborators []ShipPerson
	var collaboratorProjects []string
	if c, present := p["collaborators"]; present && c != nil {
		list, isArray := c.([]any)
		if !isArray || len(list) > 10 { // most people a single ship can carry
			return invalid("collaborators")
		}
		seen := map[string]bool{}
		for _, rawPerson := range list {
			person, isObject := rawPerson.(map[string]any)
			if !isObject {
				return invalid("collaborators")
			}
			if hp, present := person["hackatime_projects"]; present && hp != nil {
				projectList, isArray := hp.([]any)
				if !isArray {
					return invalid("collaborators.hackatime_projects")
				}
				for _, e := range projectList {
					s, isString := e.(string)
					if !isString {
						return invalid("collaborators.hackatime_projects")
					}
					if t := strings.TrimSpace(s); t != "" {
						if strings.EqualFold(t, "<<LAST_PROJECT>>") { // disallowed for shipping: never stored or credited
							disallowedHackatime = append(disallowedHackatime, t)
							continue
						}
						collaboratorProjects = append(collaboratorProjects, t)
					}
				}
			}
			if _, ok := trimmedNonEmpty(person["email"]); !ok {
				return invalid("collaborators.email")
			}
			if v, present := person["name"]; present {
				if _, isString := v.(string); !isString {
					return invalid("collaborators.name")
				}
			}
			if v, present := person["slack_id"]; present {
				if _, isString := v.(string); !isString {
					return invalid("collaborators.slack_id")
				}
			}
			if v, present := person["hackatime_id"]; present {
				if _, isString := v.(string); !isString {
					return invalid("collaborators.hackatime_id")
				}
			}
			programSeconds, ok := parseProgramSeconds(person)
			if !ok {
				return invalid("collaborators.program_hours")
			}
			email := strings.ToLower(strings.TrimSpace(person["email"].(string)))
			if seen[email] {
				return invalid("collaborators")
			}
			seen[email] = true
			collaborators = append(collaborators, ShipPerson{
				Email:           email,
				Name:            strings.TrimSpace(stringOr(person["name"])),
				SlackId:         strings.TrimSpace(stringOr(person["slack_id"])),
				HackatimeUserId: strings.TrimSpace(stringOr(person["hackatime_id"])),
				ProgramSeconds:  programSeconds,
			})
		}
	}
	// The tracked project set is the union of the ship-level list and every
	// collaborator's own hackatime_projects (deduped, ship-level order first), so
	// each person's differently-named projects still get their time fetched.
	seenProjects := map[string]bool{}
	for _, t := range hackatimeProjects {
		seenProjects[t] = true
	}
	for _, t := range collaboratorProjects {
		if !seenProjects[t] {
			seenProjects[t] = true
			hackatimeProjects = append(hackatimeProjects, t)
		}
	}

	collaboratorEmails := map[string]bool{}
	for _, c := range collaborators {
		collaboratorEmails[c.Email] = true
	}

	var journals []ShipJournal
	if j, present := p["journals"]; present {
		list, isArray := j.([]any)
		if !isArray || len(list) > 200 {
			return invalid("journals")
		}
		for _, rawEntry := range list {
			entry, isObject := rawEntry.(map[string]any)
			if !isObject {
				return invalid("journals")
			}
			atRaw := entry["at"]
			if atRaw == nil {
				atRaw = entry["date"]
			}
			at, ok := parseJsDate(jsToString(atRaw))
			if !ok {
				return invalid("journals.at")
			}
			var seconds int
			if raw, present := entry["seconds"]; present && raw != nil {
				seconds, ok = wholeSeconds(raw, 86400) // the same 24h bound as minutes
				if !ok {
					return invalid("journals.minutes")
				}
			} else {
				var minutes float64
				if m, isNum := entry["minutes"].(float64); isNum {
					minutes = m
				} else if h, isNum := entry["hours"].(float64); isNum {
					minutes = h * 60
				} else {
					return invalid("journals.minutes")
				}
				if minutes < 0 || minutes > 24*60 {
					return invalid("journals.minutes")
				}
				seconds = int(jsRound(minutes)) * 60
			}
			text, ok := trimmedNonEmpty(entry["text"])
			if !ok {
				return invalid("journals.text")
			}
			var email string
			if collaborators != nil {
				e, ok := trimmedNonEmpty(entry["email"])
				if !ok {
					return invalid("journals.email")
				}
				email = strings.ToLower(strings.TrimSpace(e))
				if !collaboratorEmails[email] { // every entry on a shared ship must attribute to a person
					return invalid("journals.email")
				}
			}
			markdown := sliceRunes(text, 10000)
			if md, isString := entry["markdown"].(string); isString {
				markdown = sliceRunes(md, 50000)
			}
			journals = append(journals, ShipJournal{
				At:       at,
				Seconds:  seconds,
				Text:     sliceRunes(text, 10000),
				Markdown: markdown,
				Email:    email,
			})
		}
	}

	var meta map[string]any
	if m, present := p["meta"]; present {
		obj, isObject := m.(map[string]any)
		if !isObject {
			return invalid("meta")
		}
		if len(obj) > 24 {
			return invalid("meta")
		}
		out := map[string]any{}
		for k, val := range obj {
			if val == nil {
				continue
			}
			key := sliceRunes(strings.TrimSpace(k), 80)
			if key == "" {
				continue
			}
			if list, isArray := val.([]any); isArray {
				for _, x := range list {
					if _, isObj := x.(map[string]any); isObj {
						return invalid("meta") // arrays of scalars only
					}
					if _, isArr := x.([]any); isArr {
						return invalid("meta")
					}
				}
				var strList []string
				for _, x := range list {
					s := strings.TrimSpace(sliceRunes(jsToString(x), 2000))
					if s != "" {
						strList = append(strList, s)
					}
					if len(strList) == 24 {
						break
					}
				}
				if len(strList) > 0 {
					out[key] = strList
				}
				continue
			}
			if _, isObj := val.(map[string]any); isObj {
				return invalid("meta") // keep it flat
			}
			value := strings.TrimSpace(sliceRunes(jsToString(val), 2000))
			if value != "" {
				out[key] = value
			}
		}
		if len(out) > 0 {
			meta = out
		}
	}

	var updateMessage *string
	if um, present := p["update_message"]; present && um != nil {
		s, isString := um.(string)
		if !isString {
			return invalid("update_message")
		}
		if t := strings.TrimSpace(s); t != "" {
			msg := sliceRunes(t, 2000)
			updateMessage = &msg
		}
	}
	if iu, present := p["is_update"]; present {
		if _, isBool := iu.(bool); !isBool {
			return invalid("is_update")
		}
	}
	isUpdate := p["is_update"] == true || updateMessage != nil

	// Evidence floor: at least one Hackatime project, journal entry, or
	// program-added minutes; otherwise there is nothing to credit.
	hasProgramSeconds := makerProgramSeconds > 0
	if collaborators != nil {
		hasProgramSeconds = false
		for _, c := range collaborators {
			if c.ProgramSeconds > 0 {
				hasProgramSeconds = true
			}
		}
	}
	if len(hackatimeProjects) == 0 && len(disallowedHackatime) == 0 && len(journals) == 0 && !hasProgramSeconds { // disallowed-only ships pass: they get a changes request instead
		return invalid("hackatime_projects_or_journals_or_program_hours")
	}

	track := "software"
	if isHardware {
		track = "hardware"
	}

	shippedAt, ok := parseShippedAt(p["shipped_at"])
	if !ok {
		return invalid("shipped_at")
	}

	var demoUrl *string
	if s, isString := p["demo_url"].(string); isString {
		t := strings.TrimSpace(s)
		demoUrl = &t
	}
	var thumbnailUrl *string
	if s, isString := p["thumbnail_url"].(string); isString {
		t := strings.TrimSpace(s)
		thumbnailUrl = &t
	}

	email, _ := maker["email"].(string)
	title, _ := p["title"].(string)
	description, _ := p["description"].(string)
	repoUrl, _ := p["repo_url"].(string)
	externalId, _ := p["external_id"].(string)

	return validated{
		ok: true,
		data: ShipPayload{
			ExternalId: strings.TrimSpace(externalId),
			Maker: ShipPerson{
				Email:           strings.ToLower(strings.TrimSpace(email)),
				Name:            strings.TrimSpace(stringOr(maker["name"])),
				SlackId:         strings.TrimSpace(stringOr(maker["slack_id"])),
				HackatimeUserId: strings.TrimSpace(stringOr(maker["hackatime_id"])),
				ProgramSeconds:  makerProgramSeconds,
			},
			Title:                       strings.TrimSpace(title),
			Description:                 sliceRunes(strings.TrimSpace(description), 10000),
			RepoUrl:                     NormalizeRepoUrl(strings.TrimSpace(repoUrl)),
			DemoUrl:                     demoUrl,
			ThumbnailUrl:                thumbnailUrl,
			Track:                       track,
			ShippedAt:                   shippedAt,
			Evidence:                    evidence,
			HackatimeProjects:           hackatimeProjects,
			DisallowedHackatimeProjects: disallowedHackatime,
			Journals:                    journals,
			Meta:                        meta,
			IsUpdate:                    isUpdate,
			UpdateMessage:               updateMessage,
			Collaborators:               collaborators,
		},
	}
}

func stringOr(v any) string {
	s, _ := v.(string)
	return s
}
