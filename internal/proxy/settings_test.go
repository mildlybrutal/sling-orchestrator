package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestLoadSettings(t *testing.T) {
	vars := []string{"SLING_DOMAIN", "SLING_TLS", "SLING_ACME_EMAIL", "SLING_WEBHOOK_PORT", "SLING_PUBLIC_BIND"}
	cases := []struct {
		name   string
		env    map[string]string
		want   Settings
		errHas string
	}{
		{"nothing set: local", nil, Settings{}, ""},
		{"domain: defaults", map[string]string{"SLING_DOMAIN": "app.example.com"},
			Settings{Domain: "app.example.com", TLS: "acme", WebhookPort: "9000", Bind: "0.0.0.0"}, ""},
		{"everything", map[string]string{"SLING_DOMAIN": "a.b.dev", "SLING_TLS": "internal", "SLING_ACME_EMAIL": "me@b.dev",
			"SLING_WEBHOOK_PORT": "9100", "SLING_PUBLIC_BIND": "127.0.0.1"},
			Settings{Domain: "a.b.dev", TLS: "internal", Email: "me@b.dev", WebhookPort: "9100", Bind: "127.0.0.1"}, ""},
		{"tls without domain", map[string]string{"SLING_TLS": "acme"}, Settings{}, "only apply with SLING_DOMAIN"},
		{"uppercase domain", map[string]string{"SLING_DOMAIN": "App.example.com"}, Settings{}, "lowercase hostname"},
		{"single label", map[string]string{"SLING_DOMAIN": "localhost"}, Settings{}, "lowercase hostname"},
		{"url not host", map[string]string{"SLING_DOMAIN": "https://a.com"}, Settings{}, "lowercase hostname"},
		{"bad tls", map[string]string{"SLING_DOMAIN": "a.com", "SLING_TLS": "off"}, Settings{}, "SLING_TLS"},
		{"bad email", map[string]string{"SLING_DOMAIN": "a.com", "SLING_ACME_EMAIL": "nope"}, Settings{}, "email"},
		{"bad port", map[string]string{"SLING_DOMAIN": "a.com", "SLING_WEBHOOK_PORT": "70000"}, Settings{}, "port"},
		{"bad bind", map[string]string{"SLING_DOMAIN": "a.com", "SLING_PUBLIC_BIND": "everywhere"}, Settings{}, "IP address"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, v := range vars {
				t.Setenv(v, c.env[v])
			}
			got, err := LoadSettings()
			if c.errHas != "" {
				if err == nil || !strings.Contains(err.Error(), c.errHas) {
					t.Fatalf("want error containing %q, got %+v, %v", c.errHas, got, err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, c.want)
			}
		})
	}
}

// usePublic switches this package to public mode for one test.
func usePublic(t *testing.T, s Settings) {
	t.Helper()
	oldS, oldG, oldURL := settings, gatewayIP, PublicURL
	Configure(s)
	gatewayIP = "172.18.0.1"
	t.Cleanup(func() { settings, gatewayIP, PublicURL = oldS, oldG, oldURL })
}

var pub = Settings{Domain: "app.example.com", TLS: TLSACME, Email: "me@example.com", WebhookPort: "9000", Bind: "0.0.0.0"}

func TestPublicConfig(t *testing.T) {
	usePublic(t, pub)
	if PublicURL != "https://app.example.com" || ProbeURL() != PublicURL {
		t.Fatalf("PublicURL = %q", PublicURL)
	}
	cfg := decodeConfig(t, "app-abc:3000")
	srv := []any{"apps", "http", "servers", httpServerName}

	if got := dig(t, cfg, append(srv, "listen", 0)...); got != ":443" {
		t.Errorf("listen = %v", got)
	}
	if got := dig(t, cfg, append(srv, "automatic_https")...); got != nil {
		t.Errorf("automatic HTTPS must stay on in public mode, got %v", got)
	}
	route := append(srv, "routes", 0)
	if got := dig(t, cfg, append(route, "match", 0, "host", 0)...); got != "app.example.com" {
		t.Errorf("host match = %v (it's what makes Caddy get a certificate)", got)
	}
	sub := append(route, "handle", 0, "routes")
	if got := dig(t, cfg, append(sub, 0, "match", 0, "path", 0)...); got != "/webhook" {
		t.Errorf("first subroute must be /webhook, got %v", got)
	}
	if got := dig(t, cfg, append(sub, 0, "handle", 0, "upstreams", 0, "dial")...); got != "172.18.0.1:9000" {
		t.Errorf("webhook dial = %v", got)
	}
	app := append(sub, 1, "handle", 0)
	if dig(t, cfg, append(app, "@id")...) != appRouteID || dig(t, cfg, append(app, "upstreams", 0, "dial")...) != "app-abc:3000" {
		t.Errorf("app route wrong: %v", dig(t, cfg, app...))
	}
	issuer := []any{"apps", "tls", "automation", "policies", 0, "issuers", 0}
	if dig(t, cfg, append(issuer, "module")...) != "acme" || dig(t, cfg, append(issuer, "email")...) != "me@example.com" {
		t.Errorf("issuer = %v", dig(t, cfg, issuer...))
	}
	if got := dig(t, cfg, "admin", "listen"); got != adminListen {
		t.Errorf("admin block lost in public mode: %v", got)
	}
}

func TestPublicConfigInternalCAAnd503(t *testing.T) {
	s := pub
	s.TLS, s.Email = TLSInternal, ""
	usePublic(t, s)
	cfg := decodeConfig(t, "")
	if got := dig(t, cfg, "apps", "tls", "automation", "policies", 0, "issuers", 0, "module"); got != "internal" {
		t.Errorf("issuer = %v", got)
	}
	app := []any{"apps", "http", "servers", httpServerName, "routes", 0, "handle", 0, "routes", 1, "handle", 0}
	if dig(t, cfg, append(app, "handler")...) != "static_response" || dig(t, cfg, append(app, "@id")...) != appRouteID {
		t.Errorf("503 page must carry the app @id: %v", dig(t, cfg, app...))
	}
}

func TestPublicConfigNeedsGateway(t *testing.T) {
	usePublic(t, pub)
	gatewayIP = ""
	if _, err := BuildConfig("x:1"); err == nil {
		t.Fatal("public config without the gateway must fail, not route /webhook nowhere")
	}
}

func TestLocalConfigTagsApp(t *testing.T) {
	cfg := decodeConfig(t, "app-abc:3000")
	if got := dig(t, cfg, "apps", "http", "servers", httpServerName, "routes", 0, "handle", 0, "@id"); got != appRouteID {
		t.Errorf("@id = %v", got)
	}
}

func TestFingerprint(t *testing.T) {
	if (Settings{}).fingerprint() != "local" {
		t.Error("local fingerprint must be \"local\" (it's what an unlabeled old proxy is treated as)")
	}
	other := pub
	other.Domain = "other.example.com"
	if pub.fingerprint() == other.fingerprint() || pub.fingerprint() == "local" {
		t.Error("different settings must have different fingerprints")
	}
}

func TestCurrentUpstreamByIDWithFallback(t *testing.T) {
	var paths []string
	fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/id/") {
			http.Error(w, `{"error":"unknown object ID"}`, http.StatusNotFound) // an old config without @id
			return
		}
		_ = json.NewEncoder(w).Encode("app-old:3000")
	})
	got, err := CurrentUpstream(context.Background())
	if err != nil || got != "app-old:3000" {
		t.Fatalf("got %q, %v", got, err)
	}
	if len(paths) != 2 || paths[0] != "/id/"+appRouteID+"/upstreams/0/dial" {
		t.Fatalf("want the @id path first, then the old path; got %v", paths)
	}
}

func TestCurrentUpstreamByIDNoFallbackWhenFound(t *testing.T) {
	calls := 0
	fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest) // handler found, no upstreams: 503 page
		_, _ = io.WriteString(w, `{"error":"invalid traversal path"}`)
	})
	if got, err := CurrentUpstream(context.Background()); err != nil || got != "" || calls != 1 {
		t.Fatalf("got %q, %v after %d calls", got, err, calls)
	}
}

func TestProbeTransport(t *testing.T) {
	if ProbeTransport() != nil {
		t.Error("local mode uses the default transport")
	}
	usePublic(t, pub)
	tr, ok := ProbeTransport().(*http.Transport)
	if !ok || tr.TLSClientConfig.ServerName != "app.example.com" {
		t.Fatalf("public transport must send SNI for the domain: %+v", tr)
	}
	for bind, want := range map[string]string{"0.0.0.0": "127.0.0.1", "::": "127.0.0.1", "10.0.0.5": "10.0.0.5"} {
		if got := loopbackFor(bind); got != want {
			t.Errorf("loopbackFor(%s) = %s", bind, got)
		}
	}
}

func TestLookupGateway(t *testing.T) {
	if ip, err := lookupGateway("172.18.0.1 fd00::1 \n"); err != nil || ip != "172.18.0.1" {
		t.Errorf("got %q, %v", ip, err)
	}
	if _, err := lookupGateway(" "); err == nil {
		t.Error("empty output must be an error")
	}
}

func TestWebhookListenAddr(t *testing.T) {
	if _, err := WebhookListenAddr(); err == nil {
		t.Error("local mode has no public webhook")
	}
	usePublic(t, pub)
	if addr, err := WebhookListenAddr(); err != nil || addr != "172.18.0.1:9000" {
		t.Errorf("got %q, %v", addr, err)
	}
}
