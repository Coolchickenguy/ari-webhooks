package githost

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitHub reads the GitHub REST api through a proxy that holds the tokens: this
// service only has the proxy's key. One client is shared by every fetcher, so
// all of its requests pass one limiter.
type GitHub struct {
	proxyUrl string
	apiKey   string
	host     string
	client   *http.Client
	pageSize int
	limiter  *limiter
	now      func() time.Time
	sleep    func(ctx context.Context, d time.Duration) error

	mu sync.Mutex
	// keyRejectedUntil: the proxy refused the key, so it is left alone for a while
	keyRejectedUntil time.Time
}

// NewGitHub reads repositories on host through the proxy at proxyUrl. Without
// both the url and the key the api is unused and those repositories are cloned.
// A blank host is github.com.
func NewGitHub(proxyUrl, apiKey, host string) *GitHub {
	if host == "" {
		host = "github.com"
	}
	g := &GitHub{
		proxyUrl: strings.TrimRight(strings.TrimSpace(proxyUrl), "/"),
		apiKey:   strings.TrimSpace(apiKey),
		host:     strings.ToLower(host),
		pageSize: 100, // the largest page the api serves
		now:      time.Now,
		sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case <-time.After(d):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	g.client = &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // a redirect names GitHub's own host: following it would carry the key out of the proxy
		},
	}
	g.limiter = &limiter{quota: 10, window: time.Second, now: func() time.Time { return g.now() }, sleep: func(ctx context.Context, d time.Duration) error { return g.sleep(ctx, d) }} // the proxy's default allowance, until a response advertises the real one
	return g
}

func (g *GitHub) configured() bool {
	return g != nil && g.proxyUrl != "" && g.apiKey != ""
}

// limiter keeps requests inside the proxy's per-key allowance: at most quota
// sends in any window. Each caller reserves the next free moment and sleeps
// until it, so concurrent callers queue instead of bursting.
type limiter struct {
	mu     sync.Mutex
	quota  int
	window time.Duration
	// sent holds the last quota reserved send times, oldest first
	sent         []time.Time
	blockedUntil time.Time
	now          func() time.Time
	sleep        func(ctx context.Context, d time.Duration) error
}

func (l *limiter) wait(ctx context.Context) error {
	l.mu.Lock()
	now := l.now()
	at := now
	if l.blockedUntil.After(at) {
		at = l.blockedUntil
	}
	if len(l.sent) >= l.quota {
		// a tenth of a window of slack: the proxy counts arrivals, not sends
		if free := l.sent[len(l.sent)-l.quota].Add(l.window + l.window/10); free.After(at) {
			at = free
		}
	}
	l.sent = append(l.sent, at)
	if extra := len(l.sent) - l.quota; extra > 0 {
		l.sent = l.sent[extra:]
	}
	l.mu.Unlock()
	if at.After(now) {
		return l.sleep(ctx, at.Sub(now))
	}
	return ctx.Err()
}

func (l *limiter) block(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if until := l.now().Add(d); until.After(l.blockedUntil) {
		l.blockedUntil = until
	}
}

var (
	policyQuota  = regexp.MustCompile(`\bq=(\d+)`)
	policyWindow = regexp.MustCompile(`\bw=(\d+)`)
)

// observe sizes the limiter from what the proxy advertises on every response
func (l *limiter) observe(header http.Header) {
	quota, window := 0, 0
	policy := header.Get("ratelimit-policy")
	if m := policyQuota.FindStringSubmatch(policy); m != nil {
		quota, _ = strconv.Atoi(m[1])
	} else {
		quota, _ = strconv.Atoi(header.Get("ratelimit-limit"))
	}
	if m := policyWindow.FindStringSubmatch(policy); m != nil {
		window, _ = strconv.Atoi(m[1])
	}
	l.mu.Lock()
	if quota >= 1 && quota <= 1000 {
		l.quota = quota
	}
	if window >= 1 && window <= 3600 {
		l.window = time.Duration(window) * time.Second
	}
	l.mu.Unlock()
	if header.Get("ratelimit-remaining") == "0" {
		if seconds, err := strconv.Atoi(header.Get("ratelimit-reset")); err == nil && seconds >= 0 {
			l.block(time.Duration(min(seconds, 60)) * time.Second)
		}
	}
}

type apiReply struct {
	status int
	body   []byte
	header http.Header
	// more: the body was longer than the caller asked for
	more bool
}

// the proxy's own errors: {"error":{"code":"..."}}. GitHub's errors carry
// "message" at the top level and no "error" object, which is how a forwarded
// 404 is told apart from the proxy not knowing a path.
func proxyErrorCode(body []byte) string {
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return ""
	}
	return envelope.Error.Code
}

func fromGitHub(body []byte) bool {
	var answer struct {
		Message *string `json:"message"`
	}
	return json.Unmarshal(body, &answer) == nil && answer.Message != nil
}

func githubRateLimited(status int, header http.Header, body []byte) bool {
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return false
	}
	if status == http.StatusTooManyRequests || header.Get("x-ratelimit-remaining") == "0" || header.Get("retry-after") != "" {
		return true
	}
	message := strings.ToLower(string(body))
	return strings.Contains(message, "rate limit") || strings.Contains(message, "abuse")
}

func jitter() time.Duration {
	return time.Duration(rand.Int64N(int64(250 * time.Millisecond)))
}

func (g *GitHub) keyRejected() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.now().Before(g.keyRejectedUntil)
}

// through rebuilds a redirect against the proxy: GitHub answers a renamed
// repository with its own address, and a request must never leave the proxy.
func (g *GitHub) through(location string) (string, bool) {
	u, err := url.Parse(location)
	if err != nil || u.Path == "" {
		return "", false
	}
	path := u.EscapedPath()
	if strings.HasPrefix(path, "/gh/") {
		path = strings.TrimPrefix(path, "/gh") // already the proxy's own form
	}
	target := g.proxyUrl + "/gh" + path
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}
	return target, true
}

func (g *GitHub) send(ctx context.Context, target, accept string, limit int64) (apiReply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return apiReply{}, err
	}
	req.Header.Set("accept", accept)
	req.Header.Set("x-api-key", g.apiKey)
	req.Header.Set("x-github-api-version", "2022-11-28")
	req.Header.Set("user-agent", "ari-webhooks")
	res, err := g.client.Do(req)
	if err != nil {
		return apiReply{}, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return apiReply{}, err
	}
	reply := apiReply{status: res.StatusCode, body: body, header: res.Header}
	if int64(len(body)) > limit {
		reply.body, reply.more = body[:limit], true
	}
	slog.Debug("gh proxy request", "path", req.URL.Path, "status", res.StatusCode, "cache", res.Header.Get("x-gh-proxy-cache"))
	return reply, nil
}

// get asks the proxy for one GitHub REST path. 200, and GitHub's own 404, 409
// and 451, are answers about the repository and come back as a reply. Everything
// else is the api failing to answer: the caller falls back to a clone.
func (g *GitHub) get(ctx context.Context, path string, query url.Values, accept string, limit int64) (apiReply, *apiFailure) {
	if g.keyRejected() {
		return apiReply{}, &apiFailure{reason: "proxy_key_rejected", quiet: true}
	}
	target := g.proxyUrl + "/gh" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	// what each kind of trouble has cost this request so far
	flaky, throttled, limited, hops := 0, 0, 0, 0
	unsteady := func(reason string) *apiFailure {
		flaky++
		if flaky > 2 || g.sleep(ctx, time.Duration(flaky*flaky)*300*time.Millisecond+jitter()) != nil {
			return &apiFailure{reason: reason, transient: true}
		}
		return nil
	}
	for {
		if g.limiter.wait(ctx) != nil {
			return apiReply{}, &apiFailure{reason: "unreachable", transient: true}
		}
		reply, err := g.send(ctx, target, accept, limit)
		if err != nil {
			if failure := unsteady("unreachable"); failure != nil {
				return apiReply{}, failure
			}
			continue
		}
		g.limiter.observe(reply.header)
		status := reply.status

		if status >= 300 && status < 400 {
			next, ok := g.through(reply.header.Get("location"))
			if hops++; !ok || hops > 3 {
				return apiReply{}, &apiFailure{reason: "http_" + strconv.Itoa(status)}
			}
			target = next
			continue
		}
		if status == http.StatusOK {
			return reply, nil
		}

		switch code := proxyErrorCode(reply.body); code {
		case "":
		case "RATE_LIMIT_EXCEEDED":
			if throttled++; throttled > 3 {
				return apiReply{}, &apiFailure{reason: "proxy_rate_limited", transient: true}
			}
			seconds, err := strconv.Atoi(reply.header.Get("retry-after"))
			if err != nil || seconds < 1 {
				seconds = 1
			}
			g.limiter.block(time.Duration(min(seconds, 5))*time.Second + jitter()) // the window is a second: a short pause for every caller, then on
			continue
		case "MISSING_API_KEY", "INVALID_API_KEY", "API_KEY_DISABLED":
			g.mu.Lock()
			known := g.now().Before(g.keyRejectedUntil)
			g.keyRejectedUntil = g.now().Add(10 * time.Minute) // a bad key must not cost a request per capture
			g.mu.Unlock()
			if !known {
				slog.Warn("gh proxy refused the api key, github repositories are cloned for the next 10 minutes", "code", code)
			}
			return apiReply{}, &apiFailure{reason: "proxy_" + strings.ToLower(code), quiet: true}
		case "DB_ERROR":
			if failure := unsteady("proxy_db_error"); failure != nil {
				return apiReply{}, failure
			}
			continue
		default:
			// NOT_FOUND, METHOD_NOT_ALLOWED, REQUEST_TOO_LARGE: this service built a
			// request the proxy does not serve. A bug here, never news about the repository.
			slog.Error("gh proxy rejected a request this service built", "code", code, "path", path, "status", status)
			return apiReply{}, &apiFailure{reason: "proxy_" + strings.ToLower(code), quiet: true}
		}

		switch {
		case status == http.StatusNotFound, status == http.StatusConflict, status == http.StatusUnavailableForLegalReasons:
			if !fromGitHub(reply.body) {
				// not the proxy's envelope and not GitHub's: whatever sits at the configured
				// url is not the proxy, and its 404 must not reject a ship
				slog.Error("gh proxy answered in a shape neither it nor github uses", "path", path, "status", status)
				return apiReply{}, &apiFailure{reason: "unexpected_" + strconv.Itoa(status), quiet: true}
			}
			return reply, nil
		case githubRateLimited(status, reply.header, reply.body):
			// the limit of whichever pooled token served this request, not ours: another may answer
			if limited++; limited > 1 || g.sleep(ctx, time.Second+jitter()) != nil {
				return apiReply{}, &apiFailure{reason: "rate_limited", transient: true}
			}
		case status >= 500:
			if failure := unsteady("http_" + strconv.Itoa(status)); failure != nil {
				return apiReply{}, failure
			}
		default:
			return apiReply{}, &apiFailure{reason: "http_" + strconv.Itoa(status)}
		}
	}
}

func (g *GitHub) getJson(ctx context.Context, path string, query url.Values, into any) (apiReply, *apiFailure) {
	reply, failure := g.get(ctx, path, query, "application/vnd.github+json", 64<<20) // a recursive tree of a large repository runs to megabytes
	if failure != nil || reply.status != http.StatusOK {
		return reply, failure
	}
	if reply.more || json.Unmarshal(reply.body, into) != nil {
		return reply, &apiFailure{reason: "bad_response"}
	}
	return reply, nil
}
