package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestJsonSuccessAndFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Write([]byte(`{"a": 1}`))
		case "/bad":
			w.WriteHeader(500)
			w.Write([]byte(`oops`))
		}
	}))
	defer srv.Close()

	res := Json(context.Background(), srv.URL+"/ok", Opts{})
	if !res.OK || res.Status != 200 || Num(Obj(res.Data)["a"]) != 1 {
		t.Fatalf("ok fetch got %+v", res)
	}
	res = Json(context.Background(), srv.URL+"/bad", Opts{})
	if res.OK || res.Status != 500 || res.Data != nil {
		t.Fatalf("500 with non-json body should be {ok:false, status:500, data:nil}, got %+v", res)
	}
	res = Json(context.Background(), "http://127.0.0.1:1/nothing", Opts{TimeoutMs: 500})
	if res.OK || res.Status != 0 {
		t.Fatalf("network error should be status 0, got %+v", res)
	}
}

func TestJsonRetryTransientOnly(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(429)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	res := JsonRetry(context.Background(), srv.URL, Opts{}, 2)
	if !res.OK || calls.Load() != 3 {
		t.Fatalf("expected success after 2 retries, got %+v after %d calls", res, calls.Load())
	}

	calls.Store(0)
	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(404)
	}))
	defer srv404.Close()
	res = JsonRetry(context.Background(), srv404.URL, Opts{}, 2)
	if res.Status != 404 || calls.Load() != 1 {
		t.Fatalf("4xx must not retry: %+v after %d calls", res, calls.Load())
	}
}

func TestMapLimitPreservesOrder(t *testing.T) {
	items := []int{1, 2, 3, 4, 5, 6, 7, 8}
	out := MapLimit(context.Background(), items, 3, func(_ context.Context, n, _ int) int { return n * 10 })
	for i, v := range out {
		if v != items[i]*10 {
			t.Fatalf("out[%d] = %d", i, v)
		}
	}
}

func TestCoercions(t *testing.T) {
	if Num("12.5") != 12.5 || Num("nope") != 0 || Num(nil) != 0 || Num(3.0) != 3 {
		t.Fatal("Num does not match _http.ts num()")
	}
	if Str("x") != "x" || Str(3) != "" {
		t.Fatal("Str mismatch")
	}
	if len(Arr("no")) != 0 || len(Arr([]any{1})) != 1 {
		t.Fatal("Arr mismatch")
	}
	if len(Obj([]any{})) != 0 {
		t.Fatal("Obj should return empty map for non-objects")
	}
}
