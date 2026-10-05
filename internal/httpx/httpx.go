package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Result mirrors ari's _http.ts FetchResult: never an error, Status 0 = network/timeout.
type Result struct {
	OK     bool
	Status int
	Data   any
}

type Opts struct {
	Method        string
	Headers       map[string]string
	Body          string
	TimeoutMs     int
	QuietStatuses []int // non-2xx statuses that are expected outcomes for the caller, not logged as failures
}

var client = &http.Client{}

func logUrl(u string) string {
	return strings.SplitN(u, "?", 2)[0] // API keys/ids can ride in query params
}

// Json fetches and JSON-parses with a timeout, degrading to {OK:false, Status:0} on any error.
func Json(ctx context.Context, url string, opts Opts) Result {
	timeout := 12 * time.Second
	if opts.TimeoutMs > 0 {
		timeout = time.Duration(opts.TimeoutMs) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	method := opts.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if opts.Body != "" {
		body = strings.NewReader(opts.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		slog.Warn("integration request build failed", "url", logUrl(url), "err", err)
		return Result{Status: 0}
	}
	for k, v := range opts.Headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		slog.Warn("integration fetch failed", "url", logUrl(url), "err", err)
		return Result{Status: 0}
	}
	defer res.Body.Close()
	var data any
	raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20)) // 32MB guard against a runaway upstream body
	if err == nil {
		if jsonErr := json.Unmarshal(raw, &data); jsonErr != nil {
			data = nil
		}
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		if !slices.Contains(opts.QuietStatuses, res.StatusCode) {
			slog.Warn("integration non-2xx", "url", logUrl(url), "status", res.StatusCode)
		}
		return Result{OK: false, Status: res.StatusCode, Data: data}
	}
	return Result{OK: true, Status: res.StatusCode, Data: data}
}

func isTransient(r Result) bool {
	return r.Status == 0 || r.Status == 429 || r.Status >= 500
}

// JsonRetry retries transient failures (network/timeout/429/5xx); 4xx returns immediately.
func JsonRetry(ctx context.Context, url string, opts Opts, retries int) Result {
	res := Json(ctx, url, opts)
	for i := 0; i < retries && !res.OK && isTransient(res); i++ {
		select {
		case <-ctx.Done():
			return res
		case <-time.After(400 * time.Millisecond * time.Duration((i+1)*(i+1))): // 400ms, 1.6s
		}
		res = Json(ctx, url, opts)
	}
	return res
}

// MapLimit runs fn over items with bounded concurrency, preserving order.
func MapLimit[T, R any](ctx context.Context, items []T, limit int, fn func(context.Context, T, int) R) []R {
	out := make([]R, len(items))
	if len(items) == 0 {
		return out
	}
	if limit < 1 {
		limit = 1
	}
	if limit > len(items) {
		limit = len(items)
	}
	var cursor int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range limit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				i := cursor
				cursor++
				mu.Unlock()
				if i >= len(items) {
					return
				}
				out[i] = fn(ctx, items[i], i)
			}
		}()
	}
	wg.Wait()
	return out
}

// Num coerces an unknown JSON value to a finite number (0 on failure), like _http.ts num().
func Num(v any) float64 {
	switch n := v.(type) {
	case float64:
		if math.IsInf(n, 0) || math.IsNaN(n) {
			return 0
		}
		return n
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%g", &f); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return f
		}
	}
	return 0
}

// Str returns a non-empty string or "".
func Str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// Arr always returns a slice (empty if the value is not one).
func Arr(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return nil
}

// Obj always returns a map (empty if the value is not one).
func Obj(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}
