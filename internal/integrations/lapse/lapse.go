package lapse

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/hackclub/ari-webhooks/internal/httpx"
)

type Client struct {
	BaseUrl string
	ApiKey  string
}

func (c *Client) base() string {
	return strings.TrimSuffix(strings.TrimSpace(c.BaseUrl), "/")
}

type Clip struct {
	ExternalId      string
	Name            string
	DurationSeconds float64
	ThumbnailUrl    string
	PlaybackUrl     string
	CreatedAt       time.Time
}

type Result struct {
	OK    bool // false = network / non-2xx with a configured key (preserve the cache)
	Clips []Clip
}

// FetchClips fetches timelapse clips for one Hackatime user + project key. No
// key or no base url is an authoritative empty, not a failure.
func (c *Client) FetchClips(ctx context.Context, hackatimeUserId, projectKey string) Result {
	if strings.TrimSpace(c.ApiKey) == "" || c.base() == "" {
		return Result{OK: true}
	}
	res := httpx.Json(ctx,
		c.base()+"/hackatime/timelapsesForProject?hackatimeUserId="+url.QueryEscape(hackatimeUserId)+"&projectKey="+url.QueryEscape(projectKey),
		httpx.Opts{Headers: map[string]string{"authorization": "Bearer " + c.ApiKey, "accept": "application/json"}})
	if !res.OK {
		return Result{}
	}

	// The list arrives as a bare array, {timelapses|clips: [...]}, or wrapped in
	// an {ok, data} envelope where data is the array or {count, timelapses}.
	var rows []any
	if list, isArray := res.Data.([]any); isArray {
		rows = list
	} else {
		top := httpx.Obj(res.Data)
		inner := res.Data
		if v, present := top["data"]; present {
			inner = v
		}
		if list, isArray := inner.([]any); isArray {
			rows = list
		} else {
			innerObj := httpx.Obj(inner)
			for _, candidate := range []any{innerObj["timelapses"], innerObj["clips"], top["timelapses"], top["clips"]} {
				if candidate != nil {
					rows = httpx.Arr(candidate)
					break
				}
			}
		}
	}

	clips := make([]Clip, 0, len(rows))
	for _, r := range rows {
		o := httpx.Obj(r)
		createdRaw := firstPresent(o, "createdAt", "created_at", "recordedAt")
		created := time.Unix(0, 0).UTC()
		switch v := createdRaw.(type) {
		case float64:
			created = time.UnixMilli(int64(v)).UTC()
		case string:
			if v != "" {
				if parsed, ok := parseJsDate(v); ok {
					created = parsed
				}
			}
		}
		name := firstStr(o, "name", "title", "note")
		if name == "" {
			name = "Timelapse"
		}
		clips = append(clips, Clip{
			ExternalId:      firstStr(o, "id", "externalId", "uuid"),
			Name:            name,
			DurationSeconds: firstNum(o, "duration", "durationSeconds", "length"),
			ThumbnailUrl:    firstStr(o, "thumbnailUrl", "thumbnail_url", "thumbnail"),
			PlaybackUrl:     firstStr(o, "playbackUrl", "playback_url", "url", "video_url"),
			CreatedAt:       created,
		})
	}
	return Result{OK: true, Clips: clips}
}

func firstPresent(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, present := m[k]; present && v != nil {
			return v
		}
	}
	return nil
}

func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, present := m[k]; present && v != nil {
			return httpx.Str(v)
		}
	}
	return ""
}

func firstNum(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, present := m[k]; present && v != nil {
			return httpx.Num(v)
		}
	}
	return 0
}

func parseJsDate(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if d, err := time.Parse(layout, s); err == nil {
			return d, true
		}
	}
	return time.Time{}, false
}
