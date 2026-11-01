package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// dig walks a decoded JSON document by keys and array indexes.
func dig(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("expected object at %q, got %T", k, v)
			}
			v = m[k]
		case int:
			a, ok := v.([]any)
			if !ok || k >= len(a) {
				t.Fatalf("expected array with index %d, got %T", k, v)
			}
			v = a[k]
		}
	}
	return v
}

func decodeConfig(t *testing.T, upstream string) any {
	t.Helper()
	raw, err := buildCaddyConfig(upstream)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	return v
}

// The most dangerous mistake in this file: POST /load replaces the whole
// config, so a config without the admin block silently moves Caddy's admin
// listener back to the container's localhost and Sling is locked out.
func TestEveryConfigKeepsAdminReachable(t *testing.T) {
	for _, up := range []string{"", "app-abc:3000"} {
		cfg := decodeConfig(t, up)
		if got := dig(t, cfg, "admin", "listen"); got != adminListen {
			t.Fatalf("upstream %q: admin.listen = %v, want %s", up, got, adminListen)
		}
		origins, _ := dig(t, cfg, "admin", "origins").([]any)
		if len(origins) == 0 {
			t.Fatalf("upstream %q: admin.origins missing, host requests would be rejected", up)
		}
	}
}

func TestConfigRoutesToUpstream(t *testing.T) {
	cfg := decodeConfig(t, "app-abc:3000")
	h := []any{"apps", "http", "servers", httpServerName, "routes", 0, "handle", 0}

	if got := dig(t, cfg, append(h, "handler")...); got != "reverse_proxy" {
		t.Fatalf("handler = %v", got)
	}
	if got := dig(t, cfg, append(h, "upstreams", 0, "dial")...); got != "app-abc:3000" {
		t.Fatalf("dial = %v", got)
	}
}

func TestConfigWithoutUpstreamServes503(t *testing.T) {
	cfg := decodeConfig(t, "")
	h := []any{"apps", "http", "servers", httpServerName, "routes", 0, "handle", 0}

	if got := dig(t, cfg, append(h, "handler")...); got != "static_response" {
		t.Fatalf("handler = %v", got)
	}
	if got := dig(t, cfg, append(h, "status_code")...); got != float64(503) {
		t.Fatalf("status_code = %v", got)
	}
}

func fakeAdmin(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	old := caddyAdminURL
	caddyAdminURL = srv.URL
	t.Cleanup(func() { srv.Close(); caddyAdminURL = old })
}

func TestSetUpstreamPostsFullConfigToLoad(t *testing.T) {
	var gotPath, gotType, gotBody string
	fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotType, gotBody = r.Method+" "+r.URL.Path, r.Header.Get("Content-Type"), string(b)
	})

	if err := setUpstream(context.Background(), "app-abc:3000"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /load" || gotType != "application/json" {
		t.Fatalf("request = %q (%s)", gotPath, gotType)
	}
	if !strings.Contains(gotBody, `"dial":"app-abc:3000"`) || !strings.Contains(gotBody, `"admin"`) {
		t.Fatalf("body missing upstream or admin block: %s", gotBody)
	}
}

func TestRejectedConfigSurfacesCaddysMessage(t *testing.T) {
	fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad config"}`, http.StatusBadRequest)
	})

	err := setUpstream(context.Background(), "app-abc:3000")
	if err == nil || !strings.Contains(err.Error(), "bad config") || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected error carrying status and body, got %v", err)
	}
}

func TestAdminUnreachable(t *testing.T) {
	fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {})
	caddyAdminURL = "http://127.0.0.1:1" // nothing listens here

	if err := setUpstream(context.Background(), "x:1"); err == nil {
		t.Fatal("expected an error when the admin API is down")
	}
	if _, err := currentUpstream(context.Background()); err == nil {
		t.Fatal("currentUpstream must error (not report 'nothing live') when Caddy is unreachable")
	}
}

func TestCurrentUpstream(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		want    string
		wantErr bool
	}{
		{"live", 200, `"app-abc:3000"`, "app-abc:3000", false},
		// What the real Caddy 2.8 returns when no upstream exists.
		{"nothing live (400)", 400, `{"error":"invalid traversal path"}`, "", false},
		{"nothing live (404)", 404, ``, "", false},
		{"server error", 500, `boom`, "", true},
		{"garbage", 200, `{not a string}`, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			})
			got, err := currentUpstream(context.Background())
			if (err != nil) != c.wantErr || got != c.want {
				t.Fatalf("got (%q, %v), want (%q, err=%v)", got, err, c.want, c.wantErr)
			}
		})
	}
}
