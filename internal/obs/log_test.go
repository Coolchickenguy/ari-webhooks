package obs

import (
	"testing"

	"github.com/getsentry/sentry-go"
)

func TestSanitizeEventRemovesPayloadAndQuery(t *testing.T) {
	event := &sentry.Event{Request: &sentry.Request{
		URL:         "https://ari.example/api/ingest/program",
		Data:        `{"private":"submission"}`,
		QueryString: "api_key=secret",
		Headers:     map[string]string{"X-Ari-Signature": "deadbeef", "Authorization": "Bearer token"},
	}}

	sanitizeEvent(event, nil)

	if event.Request.URL != "https://ari.example/api/ingest/program" {
		t.Fatalf("a url with no hidden prefix must stay as it is: %s", event.Request.URL)
	}
	if event.Request.Data != "" || event.Request.QueryString != "" || len(event.Request.Headers) != 0 {
		t.Fatalf("private request data remained: data=%q query=%q headers=%v", event.Request.Data, event.Request.QueryString, event.Request.Headers)
	}
}

func TestSanitizeEventHidesTokensAfterRegisteredPrefixes(t *testing.T) {
	HideTokenAfter("/api/example/")
	event := &sentry.Event{Request: &sentry.Request{URL: "https://ari.example/api/example/secret-token"}}

	sanitizeEvent(event, nil)

	if event.Request.URL != "https://ari.example/api/example/:token" {
		t.Fatalf("the token was not removed: %s", event.Request.URL)
	}
}
