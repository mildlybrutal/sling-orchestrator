package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Settings choose how the proxy faces users. The zero value is local mode:
// plain HTTP on 127.0.0.1:8080, for development and the e2e tests. With a
// Domain, Caddy serves HTTPS on 80/443 with certificates it obtains and
// renews itself, and forwards /webhook to "sling serve".
type Settings struct {
	Domain      string // "" = local mode
	TLS         string // "acme" (Let's Encrypt; default) or "internal" (Caddy's own CA, for testing)
	Email       string // optional: Let's Encrypt contact for expiry warnings
	WebhookPort string // port "sling serve" listens on for Caddy (on the sling-net gateway)
	Bind        string // host address for ports 80/443; default 0.0.0.0 (all)
}

const (
	TLSACME     = "acme"
	TLSInternal = "internal"
)

var (
	// Lowercase DNS name with at least two labels, e.g. app.example.com.
	domainPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	emailPattern  = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// LoadSettings reads the proxy settings from the environment. Every sling
// command must see the same values (Ensure refuses a proxy set up
// differently), so put them in one env file used by all of them.
func LoadSettings() (Settings, error) {
	s := Settings{
		Domain:      os.Getenv("SLING_DOMAIN"),
		TLS:         os.Getenv("SLING_TLS"),
		Email:       os.Getenv("SLING_ACME_EMAIL"),
		WebhookPort: os.Getenv("SLING_WEBHOOK_PORT"),
		Bind:        os.Getenv("SLING_PUBLIC_BIND"),
	}
	if s.Domain == "" {
		if s.TLS != "" || s.Email != "" || s.WebhookPort != "" || s.Bind != "" {
			return Settings{}, errors.New("SLING_TLS, SLING_ACME_EMAIL, SLING_WEBHOOK_PORT and SLING_PUBLIC_BIND only apply with SLING_DOMAIN set")
		}
		return Settings{}, nil
	}

	if !domainPattern.MatchString(s.Domain) {
		return Settings{}, fmt.Errorf("SLING_DOMAIN must be a lowercase hostname like app.example.com, got %q", s.Domain)
	}
	if s.TLS == "" {
		s.TLS = TLSACME
	}
	if s.TLS != TLSACME && s.TLS != TLSInternal {
		return Settings{}, fmt.Errorf("SLING_TLS must be %q or %q, got %q", TLSACME, TLSInternal, s.TLS)
	}
	if s.Email != "" && !emailPattern.MatchString(s.Email) {
		return Settings{}, fmt.Errorf("SLING_ACME_EMAIL is not an email address: %q", s.Email)
	}
	if s.WebhookPort == "" {
		s.WebhookPort = "9000"
	}
	if p, err := strconv.Atoi(s.WebhookPort); err != nil || p < 1 || p > 65535 {
		return Settings{}, fmt.Errorf("SLING_WEBHOOK_PORT must be a port number, got %q", s.WebhookPort)
	}
	if s.Bind == "" {
		s.Bind = "0.0.0.0"
	}
	if net.ParseIP(s.Bind) == nil {
		return Settings{}, fmt.Errorf("SLING_PUBLIC_BIND must be an IP address, got %q", s.Bind)
	}
	return s, nil
}

// Public reports whether this is HTTPS-on-a-domain mode.
func (s Settings) Public() bool { return s.Domain != "" }

// fingerprint is stored as a label on the proxy container, so a command run
// with different settings is refused instead of silently reconfiguring a
// live site (for example, a CLI run that forgot SLING_DOMAIN would
// otherwise switch production back to plain HTTP on loopback).
func (s Settings) fingerprint() string {
	if !s.Public() {
		return "local"
	}
	return fmt.Sprintf("https://%s tls=%s email=%s webhook=%s bind=%s", s.Domain, s.TLS, s.Email, s.WebhookPort, s.Bind)
}

var (
	settings  Settings
	gatewayIP string // sling-net's gateway: the host, as seen from containers (set by Ensure)
)

// Configure sets the proxy settings for this process. Call it once, before
// anything else in this package.
func Configure(s Settings) {
	settings = s
	if s.Public() {
		PublicURL = "https://" + s.Domain
	} else {
		PublicURL = "http://" + proxyBind + ":" + proxyHostPort
	}
}

// Current returns the settings in use.
func Current() Settings { return settings }

// ProbeURL is where Sling's own post-cutover checks send requests: the same
// URL users use.
func ProbeURL() string { return PublicURL }

// ProbeTransport makes Sling's checks reach this host's Caddy even when the
// domain's DNS points elsewhere (or nowhere yet): every connection goes to
// the local proxy, with the domain as TLS SNI and Host header.
//
// Certificates are not verified on this connection. It goes over the
// loopback interface to our own Caddy, so nothing can intercept it, and the
// check is about routing: did the NEW container answer? A certificate
// problem must not make Sling roll back a healthy app (that wouldn't fix
// the certificate). nil means "use the default transport" (local mode).
func ProbeTransport() http.RoundTripper {
	if !settings.Public() {
		return nil
	}
	target := net.JoinHostPort(loopbackFor(settings.Bind), "443")
	d := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, target)
		},
		TLSClientConfig:     &tls.Config{ServerName: settings.Domain, InsecureSkipVerify: true}, // see above
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 5 * time.Second,
		DisableKeepAlives:   true, // each probe is independent, like a new user
	}
}

// loopbackFor is the address to reach ports published on bind from this host.
func loopbackFor(bind string) string {
	if bind == "0.0.0.0" || bind == "::" || bind == "" {
		return "127.0.0.1"
	}
	return bind
}

// WebhookListenAddr is where "sling serve" must listen so Caddy (inside
// sling-net) can forward /webhook to it: the network's gateway address,
// which is the host as containers see it. It is not reachable from the
// internet: only Caddy's /webhook route is public. Requires Ensure.
func WebhookListenAddr() (string, error) {
	if !settings.Public() {
		return "", errors.New("no public webhook in local mode (set SLING_DOMAIN)")
	}
	if gatewayIP == "" {
		return "", errors.New("sling-net gateway unknown: proxy.Ensure must run first")
	}
	return net.JoinHostPort(gatewayIP, settings.WebhookPort), nil
}

// lookupGateway reads sling-net's IPv4 gateway from Docker.
func lookupGateway(out string) (string, error) {
	for _, f := range strings.Fields(out) {
		if ip := net.ParseIP(f); ip != nil && ip.To4() != nil {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("no IPv4 gateway in %q", strings.TrimSpace(out))
}
