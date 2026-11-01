package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var fastCfg = healthConfig{
	Interval:       5 * time.Millisecond,
	Deadline:       400 * time.Millisecond,
	RequestTimeout: 100 * time.Millisecond,
	RequiredStreak: 3,
}

func alwaysRunning() (bool, error) { return true, nil }

// scripted serves the given status codes in order, then repeats the last one.
func scripted(codes ...int) (*httptest.Server, *int32) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(atomic.AddInt32(&n, 1)) - 1
		if i >= len(codes) {
			i = len(codes) - 1
		}
		w.WriteHeader(codes[i])
	}))
	return srv, &n
}

func TestHealthyAfterStreak(t *testing.T) {
	srv, n := scripted(200)
	defer srv.Close()

	if err := waitHealthy(context.Background(), fastCfg, srv.URL, alwaysRunning); err != nil {
		t.Fatalf("expected healthy, got %v", err)
	}
	if got := atomic.LoadInt32(n); got != 3 {
		t.Fatalf("expected exactly 3 probes, got %d", got)
	}
}

func TestFailureResetsStreak(t *testing.T) {
	// 200, 200, 500 must NOT pass: the streak restarts and needs 3 more.
	srv, n := scripted(200, 200, 500, 200, 200, 200)
	defer srv.Close()

	if err := waitHealthy(context.Background(), fastCfg, srv.URL, alwaysRunning); err != nil {
		t.Fatalf("expected healthy, got %v", err)
	}
	if got := atomic.LoadInt32(n); got != 6 {
		t.Fatalf("expected 6 probes (streak reset by the 500), got %d", got)
	}
}

func TestAlwaysUnhealthyHitsDeadline(t *testing.T) {
	srv, _ := scripted(500)
	defer srv.Close()

	err := waitHealthy(context.Background(), fastCfg, srv.URL, alwaysRunning)
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("error should name the last result, got: %v", err)
	}
}

func TestCrashLoopFailsFast(t *testing.T) {
	// Endpoint answers 200, but the container reports exited: must fail
	// immediately rather than wait out the deadline.
	srv, _ := scripted(200)
	defer srv.Close()

	start := time.Now()
	err := waitHealthy(context.Background(), fastCfg, srv.URL,
		func() (bool, error) { return false, nil })
	if err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("expected exited error, got %v", err)
	}
	if time.Since(start) > fastCfg.Deadline/2 {
		t.Fatal("should fail fast, not wait for the deadline")
	}
}

func TestServerDown(t *testing.T) {
	srv, _ := scripted(200)
	url := srv.URL
	srv.Close() // connection refused from here on

	err := waitHealthy(context.Background(), fastCfg, url, alwaysRunning)
	if err == nil {
		t.Fatal("expected failure when nothing is listening")
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			http.Redirect(w, r, "/ok", http.StatusFound)
			return
		}
		w.WriteHeader(200) // would pass if the redirect were followed
	}))
	defer srv.Close()

	if err := waitHealthy(context.Background(), fastCfg, srv.URL+"/health", alwaysRunning); err == nil {
		t.Fatal("a redirecting /health must not count as healthy")
	}
}

func TestStatusHistoryOrder(t *testing.T) {
	d := &Deployment{}
	d.setStatus(StatusQueued)
	d.setStatus(StatusBuilding)
	d.setStatus(StatusFailed)

	s, h := d.Snapshot()
	if s != StatusFailed || len(h) != 3 || h[0].Status != StatusQueued || h[1].Status != StatusBuilding {
		t.Fatalf("unexpected history: %v %v", s, h)
	}
}

// upstreamServer answers 200 and claims to be served by the given upstream,
// the way Caddy reports it.
func upstreamServer(upstream string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upstream != "" {
			w.Header().Set(upstreamHeader, upstream)
		}
		w.WriteHeader(200)
	}))
}

func TestServedByNewUpstreamPasses(t *testing.T) {
	srv := upstreamServer("app-new:3000")
	defer srv.Close()
	if err := waitUntil(context.Background(), fastCfg, srv.URL, servedBy("app-new:3000"), alwaysRunning); err != nil {
		t.Fatalf("expected pass, got %v", err)
	}
}

func TestServedByOldUpstreamFails(t *testing.T) {
	// Healthy, but still the old version answering: must NOT pass.
	srv := upstreamServer("app-old:3000")
	defer srv.Close()
	err := waitUntil(context.Background(), fastCfg, srv.URL, servedBy("app-new:3000"), alwaysRunning)
	if err == nil || !strings.Contains(err.Error(), `served by "app-old:3000"`) {
		t.Fatalf("expected wrong-upstream failure naming the old one, got %v", err)
	}
}

func TestServedByMissingHeaderFails(t *testing.T) {
	// No header at all (e.g. the proxy config lost it): fail, don't assume.
	srv := upstreamServer("")
	defer srv.Close()
	if err := waitUntil(context.Background(), fastCfg, srv.URL, servedBy("app-new:3000"), alwaysRunning); err == nil {
		t.Fatal("a response without the upstream header must not pass")
	}
}
