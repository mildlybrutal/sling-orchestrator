package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mildlybrutal/sling-orchestrator/internal/deploy"
)

var testCfg = webhookConfig{
	Secret: []byte("test-secret-at-least-16"),
	Repo:   "akash/sling",
	Branch: "main",
}

func sign(secret []byte, body string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func pushBody(repo, ref, after string, deleted bool) string {
	return fmt.Sprintf(`{"ref":%q,"after":%q,"deleted":%v,"repository":{"full_name":%q}}`, ref, after, deleted, repo)
}

// send posts body to a handler and returns the status and what was queued.
func send(t *testing.T, method, event, sig, body string) (int, []*deploy.Deployment) {
	t.Helper()
	var got []*deploy.Deployment
	h := &webhookHandler{cfg: testCfg, submit: func(d *deploy.Deployment) { got = append(got, d) }}
	req := httptest.NewRequest(method, "/webhook", strings.NewReader(body))
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
	}
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, got
}

func TestWebhookValidPushQueuesDeploy(t *testing.T) {
	body := pushBody("akash/sling", "refs/heads/main", sha("a"), false)
	code, got := send(t, "POST", "push", sign(testCfg.Secret, body), body)
	if code != http.StatusAccepted || len(got) != 1 {
		t.Fatalf("code %d, queued %d; want 202 and 1", code, len(got))
	}
	if got[0].CommitSHA != sha("a") || got[0].RepoURL != "https://github.com/akash/sling.git" {
		t.Fatalf("queued %+v", got[0])
	}
	if got[0].AllowLocalRepo {
		t.Fatal("webhook deploys must never allow local repos")
	}
}

func TestWebhookRejectsBadSignatures(t *testing.T) {
	body := pushBody("akash/sling", "refs/heads/main", sha("a"), false)
	cases := map[string]string{
		"missing":       "",
		"wrong secret":  sign([]byte("some-other-secret-xx"), body),
		"other body":    sign(testCfg.Secret, body+" "), // signature for different bytes
		"no prefix":     strings.TrimPrefix(sign(testCfg.Secret, body), "sha256="),
		"sha1 header":   "sha1=" + strings.TrimPrefix(sign(testCfg.Secret, body), "sha256="),
		"not hex":       "sha256=zzzz",
		"truncated mac": sign(testCfg.Secret, body)[:20],
	}
	for name, sig := range cases {
		code, got := send(t, "POST", "push", sig, body)
		if code != http.StatusUnauthorized || len(got) != 0 {
			t.Errorf("%s: code %d, queued %d; want 401 and nothing", name, code, len(got))
		}
	}
}

func TestWebhookSignatureCheckedBeforeEvent(t *testing.T) {
	// An unsigned ping must not even get "pong".
	code, _ := send(t, "POST", "ping", "", `{}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("unsigned ping got %d, want 401", code)
	}
}

func TestWebhookIgnoresAndRejects(t *testing.T) {
	cases := []struct {
		name, event, body string
		want              int
	}{
		{"ping", "ping", `{"zen":"hi"}`, http.StatusOK},
		{"other event", "issues", `{}`, http.StatusOK},
		{"other branch", "push", pushBody("akash/sling", "refs/heads/dev", sha("a"), false), http.StatusOK},
		{"tag push", "push", pushBody("akash/sling", "refs/tags/main", sha("a"), false), http.StatusOK},
		{"branch deleted", "push", pushBody("akash/sling", "refs/heads/main", strings.Repeat("0", 40), true), http.StatusOK},
		{"other repo", "push", pushBody("evil/sling", "refs/heads/main", sha("a"), false), http.StatusForbidden},
		{"bad sha", "push", pushBody("akash/sling", "refs/heads/main", "; rm -rf /", false), http.StatusBadRequest},
		{"not json", "push", `ref=refs/heads/main`, http.StatusBadRequest},
	}
	for _, c := range cases {
		code, got := send(t, "POST", c.event, sign(testCfg.Secret, c.body), c.body)
		if code != c.want || len(got) != 0 {
			t.Errorf("%s: code %d, queued %d; want %d and nothing", c.name, code, len(got), c.want)
		}
	}
}

func TestWebhookMethodAndSize(t *testing.T) {
	if code, _ := send(t, "GET", "push", "", ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET got %d, want 405", code)
	}
	huge := strings.Repeat("x", maxWebhookBody+1)
	if code, _ := send(t, "POST", "push", sign(testCfg.Secret, huge), huge); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body got %d, want 413", code)
	}
}

func TestLoadWebhookConfig(t *testing.T) {
	t.Setenv("SLING_WEBHOOK_SECRET", "short")
	t.Setenv("SLING_REPO", "akash/sling")
	if _, err := loadWebhookConfig(); err == nil {
		t.Fatal("a short secret must be refused")
	}
	t.Setenv("SLING_WEBHOOK_SECRET", "0123456789abcdef0123")
	t.Setenv("SLING_REPO", "https://github.com/akash/sling")
	if _, err := loadWebhookConfig(); err == nil {
		t.Fatal("SLING_REPO must be owner/name, not a URL")
	}
	t.Setenv("SLING_REPO", "akash/sling")
	cfg, err := loadWebhookConfig()
	if err != nil || cfg.Branch != "main" || cfg.Listen != "127.0.0.1:9000" {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
}

// --- queue ---

func TestQueueLatestPushWins(t *testing.T) {
	release := make(chan struct{})
	started := make(chan string, 10)
	var mu sync.Mutex
	var ran []string
	q := newDeployQueue(func(d *deploy.Deployment) {
		started <- d.CommitSHA
		<-release
		mu.Lock()
		ran = append(ran, d.CommitSHA)
		mu.Unlock()
	})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { q.worker(stop); close(done) }()

	a, b, c := &deploy.Deployment{CommitSHA: sha("a")}, &deploy.Deployment{CommitSHA: sha("b")}, &deploy.Deployment{CommitSHA: sha("c")}
	q.submit(a)
	<-started // A is building
	q.submit(b)
	q.submit(c) // replaces B, which never started
	release <- struct{}{}
	if got := <-started; got != sha("c") {
		t.Fatalf("after A, ran %s; want C", got[:1])
	}
	release <- struct{}{}
	close(stop)
	<-done

	if s, _ := b.Snapshot(); s != deploy.StatusSuperseded {
		t.Fatalf("B status %q, want superseded", s)
	}
	if len(ran) != 2 || ran[0] != sha("a") || ran[1] != sha("c") {
		t.Fatalf("ran %v, want A then C", ran)
	}
}

func TestQueueSameCommitQueuedOnce(t *testing.T) {
	release := make(chan struct{})
	count := make(chan string, 10)
	q := newDeployQueue(func(d *deploy.Deployment) { count <- d.CommitSHA; <-release })
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { q.worker(stop); close(done) }()

	q.submit(&deploy.Deployment{CommitSHA: sha("a")})
	<-count
	first := &deploy.Deployment{CommitSHA: sha("b")}
	q.submit(first)
	q.submit(&deploy.Deployment{CommitSHA: sha("b")}) // GitHub redelivery
	if s, _ := first.Snapshot(); s == deploy.StatusSuperseded {
		t.Fatal("a duplicate must not supersede the identical queued deploy")
	}
	release <- struct{}{}
	<-count
	release <- struct{}{}
	close(stop)
	<-done
	select {
	case extra := <-count:
		t.Fatalf("B ran twice (extra %s)", extra[:1])
	default:
	}
}

func TestQueueStopFinishesCurrentDeploy(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	finished := false
	q := newDeployQueue(func(d *deploy.Deployment) { started <- struct{}{}; <-release; finished = true })
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { q.worker(stop); close(done) }()

	q.submit(&deploy.Deployment{CommitSHA: sha("a")})
	<-started
	close(stop)
	select {
	case <-done:
		t.Fatal("worker returned while a deploy was still running")
	case <-time.After(50 * time.Millisecond):
	}
	release <- struct{}{}
	<-done
	if !finished {
		t.Fatal("the running deploy must complete before the worker stops")
	}
}

func sha(c string) string { return strings.Repeat(c, 40) }
