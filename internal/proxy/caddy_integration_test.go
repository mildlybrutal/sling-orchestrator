//go:build integration

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mildlybrutal/sling-orchestrator/internal/health"
)

// Same settings the deploy pipeline uses after cutover.
var postCutover = health.Config{
	Interval:       500 * time.Millisecond,
	Deadline:       10 * time.Second,
	RequestTimeout: 2 * time.Second,
	RequiredStreak: 3,
}

// Runs the proxy code against a real Caddy binary (no Docker):
//
//	CADDY=/path/to/caddy go test -tags integration -run Caddy ./...
func TestRealCaddyCutover(t *testing.T) {
	bin := os.Getenv("CADDY")
	if bin == "" {
		t.Skip("set CADDY to a caddy binary")
	}
	boot, _ := BuildConfig("")
	f, _ := os.CreateTemp("", "boot*.json")
	f.Write(boot)
	f.Close()
	cmd := exec.Command(bin, "run", "--config", f.Name())
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	ctx := context.Background()
	PublicURL = "http://127.0.0.1:80" // no Docker port mapping here
	if err := WaitForAdmin(ctx, 10*time.Second); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(PublicURL + "/")
	if err != nil || resp.StatusCode != 503 {
		t.Fatalf("before any deploy want 503, got %v %v", resp, err)
	}
	if up, err := CurrentUpstream(ctx); err != nil || up != "" {
		t.Fatalf("want no upstream, got %q %v", up, err)
	}

	app := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	}
	a, b := app(), app()
	defer a.Close()
	defer b.Close()
	upA := strings.TrimPrefix(a.URL, "http://")
	upB := strings.TrimPrefix(b.URL, "http://")
	running := func() (bool, error) { return true, nil }

	for _, up := range []string{upA, upB} {
		if err := SetUpstream(ctx, up); err != nil {
			t.Fatal(err)
		}
		if got, _ := CurrentUpstream(ctx); got != up {
			t.Fatalf("CurrentUpstream = %q, want %q", got, up)
		}
		if err := health.WaitUntil(ctx, postCutover, PublicURL+"/health", health.ServedBy(UpstreamHeader, up), running); err != nil {
			t.Fatalf("ServedBy(%s): %v", up, err)
		}
	}

	// Now B is live: a check expecting A must fail and say why.
	quick := postCutover
	quick.Deadline = 2 * time.Second
	err = health.WaitUntil(ctx, quick, PublicURL+"/health", health.ServedBy(UpstreamHeader, upA), running)
	if err == nil || !strings.Contains(err.Error(), "served by") {
		t.Fatalf("expected wrong-upstream failure, got %v", err)
	}
	t.Logf("wrong-upstream error reads: %v", err)

	// Upstream down: Caddy answers 502 and the check must fail.
	b.Close()
	err = health.WaitUntil(ctx, quick, PublicURL+"/health", health.ServedBy(UpstreamHeader, upB), running)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected 502 failure, got %v", err)
	}
	t.Logf("dead-upstream error reads: %v", err)
}
