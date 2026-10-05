package probe

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hackclub/ari-webhooks/internal/ssrf"
)

type Result struct {
	OK bool
	// Transient means a retry might succeed (network error, 5xx); false for 4xx or unsafe URL.
	Transient bool
	// Blocked marks 401/403/429: alive but refusing our probe; never grounds for auto-reject.
	Blocked bool
	Status  int
	Error   string
}

var client = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse // a 3xx is accepted without following; also closes redirect-to-private SSRF
	},
}

var ipv4Literal = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)

func isLive(status int) bool {
	return status >= 200 && status < 400 // 3xx accepted without following
}

func isBlocked(status int) bool {
	return status == 401 || status == 403 || status == 429
}

// AllowPrivateHosts bypasses the SSRF guard for httptest targets. TEST ONLY.
var AllowPrivateHosts = false

func guardUrl(ctx context.Context, rawUrl string) (safe, transient bool) {
	if AllowPrivateHosts {
		return true, false
	}
	if !ssrf.IsSafeUrl(rawUrl) {
		return false, false
	}
	u, err := url.Parse(rawUrl)
	if err != nil {
		return false, false
	}
	hostname := strings.Trim(u.Hostname(), "[]")
	// Only genuine literals skip DNS; a hostname like 1.attacker.com must still resolve.
	if ipv4Literal.MatchString(hostname) || strings.Contains(hostname, ":") {
		return true, false
	}
	dns := ssrf.ResolvesSafely(ctx, hostname)
	if !dns.Safe {
		return false, dns.Transient
	}
	return true, false
}

// Url ports ari's probeUrl: HEAD first (no body), GET fallback on 405 or any
// HEAD failure (naive servers hang on HEAD while GET serves fine).
func Url(ctx context.Context, rawUrl string) Result {
	safe, transient := guardUrl(ctx, rawUrl)
	if !safe {
		errText := "unsafe_url"
		if transient {
			errText = "dns_error"
		}
		return Result{Transient: transient, Error: errText}
	}

	if res, err := send(ctx, http.MethodHead, rawUrl); err == nil {
		if isLive(res) {
			return Result{OK: true, Status: res}
		}
		if res != 405 { // 405 = HEAD unsupported, fall through to GET
			return Result{Transient: res >= 500, Blocked: isBlocked(res), Status: res, Error: "http_" + strconv.Itoa(res)}
		}
	}

	res, err := send(ctx, http.MethodGet, rawUrl)
	if err != nil {
		return Result{Transient: true, Error: "network_error: " + err.Error()}
	}
	if isLive(res) {
		return Result{OK: true, Status: res}
	}
	return Result{Transient: res >= 500, Blocked: isBlocked(res), Status: res, Error: "http_" + strconv.Itoa(res)}
}

func send(ctx context.Context, method, rawUrl string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawUrl, nil)
	if err != nil {
		return 0, err
	}
	// Browser-like headers: many hosts answer 403 to a bare client UA from a
	// datacenter IP while the page is perfectly alive in a browser.
	req.Header.Set("user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	res, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	res.Body.Close()
	return res.StatusCode, nil
}
