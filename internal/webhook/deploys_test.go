package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mildlybrutal/sling-orchestrator/internal/deploy"
)

const testToken = "tok-0123456789abcdef"

func newDeploysHandler(token string, live func(context.Context) (string, error)) (*deploysHandler, *recentDeploys) {
	r := &recentDeploys{max: 3}
	return &deploysHandler{token: []byte(token), recent: r, live: live}, r
}

func getDeploys(h http.Handler, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/deploys", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func liveIs(up string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return up, nil }
}

func TestDeploysDisabledWithoutToken(t *testing.T) {
	h, _ := newDeploysHandler("", liveIs(""))
	if rec := getDeploys(h, "Bearer anything"); rec.Code != http.StatusNotFound {
		t.Fatalf("no token configured: want 404, got %d", rec.Code)
	}
}

func TestDeploysAuth(t *testing.T) {
	h, _ := newDeploysHandler(testToken, liveIs(""))
	for _, auth := range []string{"", "Bearer", "Bearer wrong", "bearer " + testToken, "Basic " + testToken, "Bearer " + testToken + "x"} {
		if rec := getDeploys(h, auth); rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q: want 401, got %d", auth, rec.Code)
		}
	}
	if rec := getDeploys(h, "Bearer "+testToken); rec.Code != http.StatusOK {
		t.Errorf("valid token: want 200, got %d", rec.Code)
	}
}

func TestDeploysMethod(t *testing.T) {
	h, _ := newDeploysHandler(testToken, liveIs(""))
	req := httptest.NewRequest(http.MethodPost, "/deploys", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: want 405, got %d", rec.Code)
	}
}

func TestDeploysBody(t *testing.T) {
	h, recent := newDeploysHandler(testToken, liveIs("app-"+sha("b")+":3000"))
	a, b := &deploy.Deployment{CommitSHA: sha("a")}, &deploy.Deployment{CommitSHA: sha("b")}
	a.SetStatus(deploy.StatusQueued)
	a.SetStatus(deploy.StatusSuperseded)
	b.SetStatus(deploy.StatusQueued)
	recent.add(a)
	recent.add(b)

	rec := getDeploys(h, "Bearer "+testToken)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var got deploysResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, rec.Body)
	}
	if got.Live != "app-"+sha("b")+":3000" || len(got.Deploys) != 2 {
		t.Fatalf("got %+v", got)
	}
	if d := got.Deploys[0]; d.Commit != sha("b") || d.Status != deploy.StatusQueued || d.Kind != "deploy" {
		t.Errorf("newest first: got %+v", d)
	}
	if d := got.Deploys[1]; d.Status != deploy.StatusSuperseded || len(d.Timeline) != 2 {
		t.Errorf("older entry: got %+v", d)
	}
}

func TestDeploysLiveError(t *testing.T) {
	h, _ := newDeploysHandler(testToken, func(context.Context) (string, error) {
		return "", errors.New("caddy admin API unreachable")
	})
	var got deploysResponse
	_ = json.Unmarshal(getDeploys(h, "Bearer "+testToken).Body.Bytes(), &got)
	if !strings.Contains(got.LiveError, "unreachable") {
		t.Fatalf("want live_error, got %+v", got)
	}
}

func TestRecentDeploysKeepsNewest(t *testing.T) {
	r := &recentDeploys{max: 3}
	for i := 0; i < 5; i++ {
		r.add(&deploy.Deployment{CommitSHA: fmt.Sprint(i)})
	}
	infos := r.infos()
	if len(infos) != 3 || infos[0].Commit != "4" || infos[2].Commit != "2" {
		t.Fatalf("want 4,3,2; got %+v", infos)
	}
}

func TestLoadWebhookConfigStatusToken(t *testing.T) {
	t.Setenv("SLING_WEBHOOK_SECRET", "0123456789abcdef0123")
	t.Setenv("SLING_REPO", "akash/sling")
	t.Setenv("SLING_STATUS_TOKEN", "short")
	if _, err := loadWebhookConfig(); err == nil {
		t.Fatal("a short status token must be refused")
	}
	t.Setenv("SLING_STATUS_TOKEN", testToken)
	if cfg, err := loadWebhookConfig(); err != nil || string(cfg.StatusToken) != testToken {
		t.Fatalf("%+v %v", cfg, err)
	}
}
