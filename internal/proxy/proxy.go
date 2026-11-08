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

	proxyStartTimeout = 3 * time.Minute
	dockerTimeout     = 30 * time.Second
)

var (
	caddyAdminURL = "http://127.0.0.1:" + adminHostPort
	// PublicURL is where users (and post-cutover checks) reach the proxy.
	PublicURL   = "http://" + proxyBind + ":" + proxyHostPort
	adminClient = &http.Client{Timeout: 10 * time.Second}
)

// BuildConfig returns the whole Caddy JSON config for upstream.
func BuildConfig(upstream string) ([]byte, error) {
	var handler map[string]any

	if upstream == "" {
		handler = map[string]any{
			"handler":     "static_response",
			"status_code": 503,
			"body":        "sling: no app deployed yet\n",
		}

	} else {
		handler = map[string]any{
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

	cfg := map[string]any{
		"admin": map[string]any{
			"listen":  adminListen,
			"origins": []string{"localhost:" + adminHostPort, "127.0.0.1:" + adminHostPort},
		},
		"apps": map[string]any{
			"http": map[string]any{
				"servers": map[string]any{
					httpServerName: map[string]any{
						"listen":          []string{":80"},
						"automatic_https": map[string]any{"disable": true},
						"routes": []any{
							map[string]any{"handle": []any{handler}},
						},
					},
				},
			},
		},
	}
	return json.Marshal(cfg)
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
func CurrentUpstream(ctx context.Context) (string, error) {
	url := caddyAdminURL + "/config/apps/http/servers/" + httpServerName + "/routes/0/handle/0/upstreams/0/dial"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := adminClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("caddy admin API unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	switch {
	case resp.StatusCode == http.StatusOK:
		var dial string
		if err := json.Unmarshal(body, &dial); err != nil {
			return "", fmt.Errorf("unexpected upstream value %q: %w", strings.TrimSpace(string(body)), err)
		}
		return dial, nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// The path does not exist: the config has no upstream (503 page).
		return "", nil
	default:
		return "", fmt.Errorf("caddy admin API: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
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

	state, err := run.Capture(dockerTimeout, "", "docker", "inspect", "-f", "{{.State.Status}}", proxyName)
	switch {
	case err != nil:
		// Not created yet: start it with a "no app yet" config.
		boot, err := BuildConfig("")
		if err != nil {
			return err
		}
		fmt.Printf("Starting proxy container %s (first run pulls %s)...\n", proxyName, proxyImage)
		_, err = run.Capture(proxyStartTimeout, "", "docker", "run", "-d",
			"--name", proxyName,
			"--restart", "unless-stopped",
			"--network", NetworkName,
			"-p", fmt.Sprintf("%s:%s:80", proxyBind, proxyHostPort),
			"-p", fmt.Sprintf("127.0.0.1:%s:%s", adminHostPort, adminHostPort),
			"-e", "SLING_BOOT="+string(boot),
			proxyImage,
			"sh", "-c", `printf %s "$SLING_BOOT" > /tmp/boot.json && exec caddy run --config /tmp/boot.json --resume`,
		)
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
