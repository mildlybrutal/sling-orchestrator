//go:build integration

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Kill the new version during the grace window: watch must fail and
// rollback must put the old version back, through a real Caddy.
func TestRealCaddyGraceRollback(t *testing.T) {
	bin := os.Getenv("CADDY")
	if bin == "" {
		t.Skip("set CADDY to a caddy binary")
	}
	boot, _ := buildCaddyConfig("")
	f, _ := os.CreateTemp("", "boot*.json")
	f.Write(boot)
	f.Close()
	cmd := exec.Command(bin, "run", "--config", f.Name())
	cmd.Start()
	defer cmd.Process.Kill()
	ctx := context.Background()
	proxyPublicURL = "http://127.0.0.1:80"
	if err := waitForAdmin(ctx, 10*time.Second); err != nil {
		t.Fatal(err)
	}

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	old, nw := httptest.NewServer(ok), httptest.NewServer(ok)
	defer old.Close()
	upOld := strings.TrimPrefix(old.URL, "http://")
	upNew := strings.TrimPrefix(nw.URL, "http://")
	running := func() (bool, error) { return true, nil }

	setUpstream(ctx, upOld)
	setUpstream(ctx, upNew)
	if err := waitUntil(ctx, postCutoverHealth, proxyPublicURL+"/health", servedBy(upNew), running); err != nil {
		t.Fatal(err)
	}

	time.AfterFunc(1*time.Second, nw.Close) // new version dies mid-window
	start := time.Now()
	err := watch(ctx, postCutoverHealth, proxyPublicURL+"/health", servedBy(upNew), running, 30*time.Second, 3)
	if err == nil {
		t.Fatal("watch should fail when the new version dies")
	}
	t.Logf("watch failed after %v: %v", time.Since(start).Round(time.Millisecond), err)

	d := &Deployment{}
	rbErr := rollback(ctx, d, upOld, errors.New("failed during grace window: "+err.Error()))
	t.Logf("rollback returned: %v", rbErr)
	if s, _ := d.Snapshot(); s != StatusRolledBack {
		t.Fatalf("status = %s, want rolled_back", s)
	}
	if up, _ := currentUpstream(ctx); up != upOld {
		t.Fatalf("proxy points at %q, want old %q", up, upOld)
	}
	if err := waitUntil(ctx, postCutoverHealth, proxyPublicURL+"/health", servedBy(upOld), running); err != nil {
		t.Fatalf("old version not serving after rollback: %v", err)
	}
}
