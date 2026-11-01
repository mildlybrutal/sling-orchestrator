package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	networkName = "sling-net"
	proxyName   = "sling-caddy"
	proxyImage  = "caddy:2.11.7-alpine"
	appPort     = "3000"

	proxyBind     = "127.0.0.1"
	proxyHostPort = "8080"

	adminListen    = "0.0.0.0:2019"
	adminHostPort  = "2019"
	httpServerName = "sling"

	proxyStartTimeout = 3 * time.Minute
)

var (
	caddyAdminURL  = "http://127.0.0.1:" + adminHostPort
	proxyPublicURL = "http://" + proxyBind + ":" + proxyHostPort
	adminClient    = &http.Client{Timeout: 10 * time.Second}

	postCutoverHealth = healthConfig{
		Interval:       500 * time.Millisecond,
		Deadline:       10 * time.Second,
		RequestTimeout: 2 * time.Second,
		RequiredStreak: 3,
	}
)

func buildCaddyConfig(upstream string) ([]byte, error) {
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

// points all traffic to upstream
func setUpstream(ctx context.Context, upstream string) error {
	cfg, err := buildCaddyConfig(upstream)
	if err != nil {
		return err
	}
	return caddyLoad(ctx, cfg)
}

func currentUpstream(ctx context.Context) (string, error) {
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
	if _, err := runCaptureCmd(dockerTimeout, "", "docker", "network", "inspect", networkName); err == nil {
		return nil
	}
	if _, err := runCaptureCmd(dockerTimeout, "", "docker", "network", "create", networkName); err != nil {
		return fmt.Errorf("creating network %s: %w", networkName, err)
	}
	return nil
}

//It makes sure that network and Caddy container exist and that Caddy admin API answers, runs after every deploy - safety

func ensureProxy(ctx context.Context) error {
	if err := ensureNetwork(); err != nil {
		return err
	}

	state, err := runCaptureCmd(dockerTimeout, "", "docker", "inspect", "-f", "{{.State.Status}}", proxyName)
	switch {
	case err != nil:
		// Not created yet: start it with a "no app yet" config.
		boot, err := buildCaddyConfig("")
		if err != nil {
			return err
		}
		fmt.Printf("Starting proxy container %s (first run pulls %s)...\n", proxyName, proxyImage)
		_, err = runCaptureCmd(proxyStartTimeout, "", "docker", "run", "-d",
			"--name", proxyName,
			"--restart", "unless-stopped",
			"--network", networkName,
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
		if _, err := runCaptureCmd(dockerTimeout, "", "docker", "start", proxyName); err != nil {
			return fmt.Errorf("restarting proxy: %w", err)
		}
	}

	return waitForAdmin(ctx, 20*time.Second)
}

func waitForAdmin(ctx context.Context, limit time.Duration) error {
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
