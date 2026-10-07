package githost

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/hackclub/ari-webhooks/internal/ssrf"
)

var (
	ipv4Literal  = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)
	infoRefsTail = regexp.MustCompile(`/info/refs(\?.*)?$`)
)

func newRedirectClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

var (
	redirectClient            = newRedirectClient(ssrf.Transport()) // every hop is re-vetted at dial time, so a name that rebinds after the DNS check still cannot reach the internal network
	privateHostRedirectClient = newRedirectClient(http.DefaultTransport)
)

func (f *Fetcher) urlIsSafeResolved(ctx context.Context, raw string) bool {
	if f.AllowPrivateHosts {
		return true
	}
	if !ssrf.IsSafeUrl(raw) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	hostname := strings.Trim(u.Hostname(), "[]")
	if ipv4Literal.MatchString(hostname) || strings.Contains(hostname, ":") {
		return true
	}
	return ssrf.ResolvesSafely(ctx, hostname).Safe
}

// resolveRedirectedBase walks the smart-HTTP /info/refs redirect chain with the
// full SSRF guard on every hop, so the redirect-disabled clone still reaches
// hosts that 30x to their git backend. Best-effort: failures return the original.
func (f *Fetcher) resolveRedirectedBase(ctx context.Context, repoUrl string) string {
	base := strings.TrimSuffix(repoUrl, "/")
	for hop := 0; hop < 4; hop++ {
		discovery := base + "/info/refs?service=git-upload-pack"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, discovery, nil)
		if err != nil {
			return repoUrl
		}
		client := redirectClient
		if f.AllowPrivateHosts {
			client = privateHostRedirectClient // the httptest server is loopback, which the vetted dialer refuses
		}
		res, err := client.Do(req)
		if err != nil {
			return repoUrl
		}
		res.Body.Close()
		if res.StatusCode < 300 || res.StatusCode >= 400 {
			break
		}
		location := res.Header.Get("location")
		if location == "" {
			break
		}
		locationUrl, err := url.Parse(location)
		if err != nil {
			break
		}
		discoveryUrl, _ := url.Parse(discovery)
		next := infoRefsTail.ReplaceAllString(discoveryUrl.ResolveReference(locationUrl).String(), "")
		if next == base {
			break
		}
		if !f.urlIsSafeResolved(ctx, next) {
			return repoUrl // redirect into unsafe space: clone the original
		}
		base = next
	}
	return base
}

// guardRepoUrl is the SSRF policy every fetch shares: scheme and literal-address
// checks first, then a resolver check that separates transient DNS failures from
// genuinely unsafe hosts.
func (f *Fetcher) guardRepoUrl(ctx context.Context, repoUrl string) (code string, transient bool, ok bool) {
	if !f.AllowPrivateHosts && !ssrf.IsSafeUrl(repoUrl) {
		return "unsafe_url", false, false
	}
	u, err := url.Parse(repoUrl)
	if err != nil {
		return "unsafe_url", false, false
	}
	hostname := strings.Trim(u.Hostname(), "[]")
	if !f.AllowPrivateHosts && !ipv4Literal.MatchString(hostname) && !strings.Contains(hostname, ":") {
		dns := ssrf.ResolvesSafely(ctx, hostname)
		if !dns.Safe {
			// Transient DNS failures are not "unsafe URL": report clone_failed so
			// the caller's retry window applies.
			code := "unsafe_url"
			if dns.Transient {
				code = "clone_failed"
			}
			return code + ": " + dns.Reason, dns.Transient, false
		}
	}
	return "", false, true
}
