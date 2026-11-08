// Package webhook runs "sling serve": it checks signed GitHub push deliveries
// and feeds their commits into a queue where the latest push wins.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/mildlybrutal/sling-orchestrator/internal/deploy"
)

// GitHub caps webhook payloads at 25 MB, but a push event is a few KB. Read
// at most this much before the signature has been checked.
const maxWebhookBody = 5 << 20

var (
	repoNamePattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	branchNamePattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

type webhookConfig struct {
	Secret []byte // shared with GitHub; signs every delivery
	Repo   string // the one repository allowed to deploy, "owner/name"
	Branch string // the one branch that deploys
	Listen string // where the webhook server listens

	// StatusToken guards GET /deploys. Optional: unset disables the endpoint.
	StatusToken []byte
}

// loadWebhookConfig reads the server's settings from the environment. The
// secret is required: without it anyone who finds the URL could deploy.
func loadWebhookConfig() (webhookConfig, error) {
	cfg := webhookConfig{
		Secret: []byte(os.Getenv("SLING_WEBHOOK_SECRET")),
		Repo:   os.Getenv("SLING_REPO"),
		Branch: envOr("SLING_BRANCH", "main"),
		Listen: envOr("SLING_LISTEN", "127.0.0.1:9000"),

		StatusToken: []byte(os.Getenv("SLING_STATUS_TOKEN")),
	}
	if len(cfg.Secret) < 16 {
		return cfg, errors.New("SLING_WEBHOOK_SECRET must be set to at least 16 characters (try: openssl rand -hex 32)")
	}
	if n := len(cfg.StatusToken); n > 0 && n < 16 {
		return cfg, errors.New("SLING_STATUS_TOKEN must be at least 16 characters, or unset (try: openssl rand -hex 32)")
	}
	if !repoNamePattern.MatchString(cfg.Repo) {
		return cfg, fmt.Errorf("SLING_REPO must be \"owner/name\", got %q", cfg.Repo)
	}
	if !branchNamePattern.MatchString(cfg.Branch) {
		return cfg, fmt.Errorf("invalid SLING_BRANCH %q", cfg.Branch)
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// cloneURL is built from Sling's own config, never from the payload: the
// payload only gets to choose WHICH commit, not where code comes from.
func (c webhookConfig) cloneURL() string {
	return "https://github.com/" + c.Repo + ".git"
}

// The only fields Sling reads from a push event.
type pushEvent struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type webhookHandler struct {
	cfg    webhookConfig
	submit func(*deploy.Deployment)
}

// ServeHTTP checks, in this order: method, size, signature, event, repo,
// branch, commit. Nothing in the body is trusted until the signature over
// the exact bytes received has been verified.
func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		reply(w, http.StatusMethodNotAllowed, "POST only")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			reply(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		reply(w, http.StatusBadRequest, "could not read body")
		return
	}

	delivery := r.Header.Get("X-GitHub-Delivery")
	if !validSignature(h.cfg.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		log.Printf("webhook %q: rejected, bad or missing signature (from %s)", delivery, r.RemoteAddr)
		reply(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	switch event := r.Header.Get("X-GitHub-Event"); event {
	case "ping":
		reply(w, http.StatusOK, "pong")
		return
	case "push":
	default:
		reply(w, http.StatusOK, fmt.Sprintf("ignored: event %q", event))
		return
	}

	var ev pushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		reply(w, http.StatusBadRequest, "body is not a JSON push event (set the webhook content type to application/json)")
		return
	}
	if ev.Repository.FullName != h.cfg.Repo {
		// Correctly signed, so this is our secret on another repo's webhook.
		log.Printf("webhook %q: rejected, repo %q is not %q", delivery, ev.Repository.FullName, h.cfg.Repo)
		reply(w, http.StatusForbidden, "repository not allowed")
		return
	}
	if ev.Ref != "refs/heads/"+h.cfg.Branch {
		reply(w, http.StatusOK, fmt.Sprintf("ignored: %s is not %s", ev.Ref, h.cfg.Branch))
		return
	}
	if ev.Deleted {
		reply(w, http.StatusOK, "ignored: branch deleted")
		return
	}
	if !deploy.ValidSHA(ev.After) {
		reply(w, http.StatusBadRequest, "invalid commit SHA")
		return
	}

	h.submit(&deploy.Deployment{RepoURL: h.cfg.cloneURL(), CommitSHA: ev.After})
	log.Printf("webhook %q: queued %s", delivery, ev.After[:7])
	// GitHub gives up after 10 s, and a deploy takes longer. Answer now,
	// deploy in the background.
	reply(w, http.StatusAccepted, "queued "+ev.After[:7])
}

// validSignature checks GitHub's X-Hub-Signature-256 header: "sha256=" plus
// the hex HMAC-SHA256 of the raw body under the shared secret. hmac.Equal
// compares in constant time, so response timing doesn't leak how many
// leading bytes of a forged signature were right.
func validSignature(secret, body []byte, header string) bool {
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func reply(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintln(w, msg)
}
