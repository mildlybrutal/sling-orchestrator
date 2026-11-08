// Package proxy runs the Caddy reverse proxy in front of the apps and switches
// its upstream through the admin API. One config is built from one value
// (the upstream), so boot, cutover and rollback all use the same code.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mildlybrutal/sling-orchestrator/internal/run"
)

const (
	// NetworkName is the Docker network Caddy and every app container join.
	NetworkName = "sling-net"
	proxyName   = "sling-caddy"
	proxyImage  = "caddy:2.11.7-alpine"
	// AppPort is the port every app must listen on inside its container.
	AppPort = "3000"

	proxyBind     = "127.0.0.1"
	proxyHostPort = "8080"

	adminListen    = "0.0.0.0:2019"
	adminHostPort  = "2019"
	httpServerName = "sling"

	// appRouteID tags the handler that serves the app (or the 503 page), so
	// Sling can find it in any config shape: GET /id/sling_app/...
	appRouteID = "sling_app"

	// Caddy keeps certificates and ACME account keys in /data. A named
	// volume keeps them when the container is recreated, so Sling never
	// re-requests certificates it already has (Let's Encrypt rate-limits that).
	dataVolume = "sling-caddy-data"
	// The label holding Settings.fingerprint() on the proxy container.
	settingsLabel = "sling.proxy"

	proxyStartTimeout = 3 * time.Minute
	dockerTimeout     = 30 * time.Second
)

var (
	caddyAdminURL = "http://127.0.0.1:" + adminHostPort
	// PublicURL is where users (and post-cutover checks) reach the proxy.
	PublicURL   = "http://" + proxyBind + ":" + proxyHostPort
	adminClient = &http.Client{Timeout: 10 * time.Second}
)

// BuildConfig returns the whole Caddy JSON config for upstream, in the mode
// set by Configure. POST /load replaces everything, so every config carries
// the admin block, the TLS policy and the webhook route.
func BuildConfig(upstream string) ([]byte, error) {
	var handler map[string]any

	if upstream == "" {
		handler = map[string]any{
			"@id":         appRouteID,
			"handler":     "static_response",
			"status_code": 503,
			"body":        "sling: no app deployed yet\n",
		}

	} else {
		handler = map[string]any{
			"@id":       appRouteID,
			"handler":   "reverse_proxy",
			"upstreams": []any{map[string]any{"dial": upstream}},
			"headers": map[string]any{
				"response": map[string]any{
					"set": map[string][]string{
						UpstreamHeader: {"{http.reverse_proxy.upstream.hostport}"},
					},
				},
			},
		}
	}

	admin := map[string]any{
		"listen":  adminListen,
		"origins": []string{"localhost:" + adminHostPort, "127.0.0.1:" + adminHostPort},
	}
	appRoute := map[string]any{"handle": []any{handler}}

	if !settings.Public() {
		return json.Marshal(map[string]any{
			"admin": admin,
			"apps": map[string]any{"http": map[string]any{"servers": map[string]any{
				httpServerName: map[string]any{
					"listen":          []string{":80"},
					"automatic_https": map[string]any{"disable": true},
					"routes":          []any{appRoute},
				},
			}}},
		})
	}

	if gatewayIP == "" {
		return nil, fmt.Errorf("public mode needs sling-net's gateway: call proxy.Ensure first")
	}
	// Public: HTTPS on 443 for exactly one host. Caddy's automatic HTTPS sees
	// the host matcher, obtains a certificate for it, and adds the :80
	// listener itself (ACME HTTP challenge + redirect to https).
	webhookRoute := map[string]any{
		"match": []any{map[string]any{"path": []string{"/webhook"}}},
		"handle": []any{map[string]any{
			"handler":   "reverse_proxy",
			"upstreams": []any{map[string]any{"dial": net.JoinHostPort(gatewayIP, settings.WebhookPort)}},
		}},
	}
	issuer := map[string]any{"module": "internal"}
	if settings.TLS == TLSACME {
		issuer = map[string]any{"module": "acme"}
		if settings.Email != "" {
			issuer["email"] = settings.Email
		}
	}
	return json.Marshal(map[string]any{
		"admin": admin,
		"apps": map[string]any{
			"http": map[string]any{"servers": map[string]any{
				httpServerName: map[string]any{
					"listen": []string{":443"},
					"routes": []any{map[string]any{
						"match": []any{map[string]any{"host": []string{settings.Domain}}},
						"handle": []any{map[string]any{
							"handler": "subroute",
							"routes":  []any{webhookRoute, appRoute},
						}},
						"terminal": true,
					}},
				},
			}},
			"tls": map[string]any{"automation": map[string]any{"policies": []any{
				map[string]any{"subjects": []string{settings.Domain}, "issuers": []any{issuer}},
			}}},
		},
	})
}

func caddyLoad(ctx context.Context, cfg []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, caddyAdminURL+"/load", bytes.NewReader(cfg))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := adminClient.Do(req)

	if err != nil {
		return fmt.Errorf("caddy admin API unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("caddy rejected config: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// SetUpstream points all traffic at upstream ("app-<sha>:3000"), or at the
// "no app deployed yet" 503 page when upstream is "".
func SetUpstream(ctx context.Context, upstream string) error {
	cfg, err := BuildConfig(upstream)
	if err != nil {
		return err
	}
	return caddyLoad(ctx, cfg)
}

// CurrentUpstream returns what Caddy is routing to now, or "" for the 503 page.
// It finds the app handler by its @id, so it works in every mode. A config
// written before handlers had an @id (a proxy from an older Sling, kept by
// --resume) has none, so it falls back to the old fixed path once.
func CurrentUpstream(ctx context.Context) (string, error) {
	dial, found, err := readDial(ctx, caddyAdminURL+"/id/"+appRouteID+"/upstreams/0/dial")
	if err != nil || found {
		return dial, err
	}
	dial, _, err = readDial(ctx, caddyAdminURL+"/config/apps/http/servers/"+httpServerName+"/routes/0/handle/0/upstreams/0/dial")
	return dial, err
}

// readDial GETs one dial value. found is false when the admin API says the
// path or ID doesn't exist (404); a 400 means the handler exists but has no
// upstream, which is the 503 page: found, nothing live.
func readDial(ctx context.Context, url string) (dial string, found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", false, err
	}
	resp, err := adminClient.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("caddy admin API unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	switch {
	case resp.StatusCode == http.StatusOK:
		if err := json.Unmarshal(body, &dial); err != nil {
			return "", true, fmt.Errorf("unexpected upstream value %q: %w", strings.TrimSpace(string(body)), err)
		}
		return dial, true, nil
	case resp.StatusCode == http.StatusNotFound:
		return "", false, nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// The handler has no upstreams: the 503 page.
		return "", true, nil
	default:
		return "", true, fmt.Errorf("caddy admin API: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

func ensureNetwork() error {
	if _, err := run.Capture(dockerTimeout, "", "docker", "network", "inspect", NetworkName); err == nil {
		return nil
	}
	if _, err := run.Capture(dockerTimeout, "", "docker", "network", "create", NetworkName); err != nil {
		return fmt.Errorf("creating network %s: %w", NetworkName, err)
	}
	return nil
}

// Ensure makes sure sling-net and the Caddy container exist and that the admin
// API answers. It runs at the start of every deploy, so a broken proxy fails
// the deploy before any build time is spent.
func Ensure(ctx context.Context) error {
	if err := ensureNetwork(); err != nil {
		return err
	}
	if settings.Public() {
		out, err := run.Capture(dockerTimeout, "", "docker", "network", "inspect", NetworkName,
			"-f", "{{range .IPAM.Config}}{{.Gateway}} {{end}}")
		if err != nil {
			return fmt.Errorf("reading %s gateway: %w", NetworkName, err)
		}
		if gatewayIP, err = lookupGateway(out); err != nil {
			return fmt.Errorf("reading %s gateway: %w", NetworkName, err)
		}
	}

	out, err := run.Capture(dockerTimeout, "", "docker", "inspect", "-f",
		`{{.State.Status}}|{{index .Config.Labels "`+settingsLabel+`"}}`, proxyName)
	state, label, _ := strings.Cut(out, "|")
	if label == "" {
		label = "local" // created by a Sling from before this label existed
	}
	if err == nil && label != settings.fingerprint() {
		return fmt.Errorf("proxy %s was set up as %q, but this command has %q.\n"+
			"Use the same SLING_DOMAIN / SLING_TLS / SLING_ACME_EMAIL / SLING_WEBHOOK_PORT / SLING_PUBLIC_BIND for every sling command,\n"+
			"or, to change mode on purpose: docker rm -f %s (the site is down until the next deploy; certificates are kept in volume %s)",
			proxyName, label, settings.fingerprint(), proxyName, dataVolume)
	}
	switch {
	case err != nil:
		// Not created yet: start it with a "no app yet" config.
		boot, err := BuildConfig("")
		if err != nil {
			return err
		}
		fmt.Printf("Starting proxy container %s (first run pulls %s)...\n", proxyName, proxyImage)
		args := []string{"run", "-d",
			"--name", proxyName,
			"--restart", "unless-stopped",
			"--network", NetworkName,
			"--label", settingsLabel + "=" + settings.fingerprint(),
			"-v", dataVolume + ":/data",
			// The admin API can rewrite all routing: loopback only, in every mode.
			"-p", fmt.Sprintf("127.0.0.1:%s:%s", adminHostPort, adminHostPort),
		}
		if settings.Public() {
			b := settings.Bind
			args = append(args,
				"-p", net.JoinHostPort(b, "80")+":80",
				"-p", net.JoinHostPort(b, "443")+":443",
				"-p", net.JoinHostPort(b, "443")+":443/udp", // HTTP/3
			)
		} else {
			args = append(args, "-p", fmt.Sprintf("%s:%s:80", proxyBind, proxyHostPort))
		}
		args = append(args,
			"-e", "SLING_BOOT="+string(boot),
			proxyImage,
			"sh", "-c", `printf %s "$SLING_BOOT" > /tmp/boot.json && exec caddy run --config /tmp/boot.json --resume`,
		)
		_, err = run.Capture(proxyStartTimeout, "", "docker", args...)
		if err != nil {
			return fmt.Errorf("starting proxy: %w", err)
		}
	case state != "running":
		if _, err := run.Capture(dockerTimeout, "", "docker", "start", proxyName); err != nil {
			return fmt.Errorf("restarting proxy: %w", err)
		}
	}

	return WaitForAdmin(ctx, 20*time.Second)
}

// WaitForAdmin polls the admin API until it answers or limit passes.
func WaitForAdmin(ctx context.Context, limit time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, caddyAdminURL+"/config/", nil)
		if err != nil {
			return err
		}
		if resp, err := adminClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("caddy admin API did not come up within %v (check: docker logs %s)", limit, proxyName)
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// Caddy stamps every proxied response with the upstream that answered it,
// so Sling can tell which version served a request without relying on
// anything inside the app.
const UpstreamHeader = "X-Sling-Upstream"
