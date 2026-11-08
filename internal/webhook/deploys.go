package webhook

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mildlybrutal/sling-orchestrator/internal/deploy"
)

// How many recent deploys GET /deploys remembers. In memory only: a restart
// forgets them (persistent history is the next roadmap step).
const recentDeploysKept = 20

// recentDeploys remembers the last few deploys the server accepted, newest
// first, including ones still waiting or running.
type recentDeploys struct {
	mu   sync.Mutex
	list []*deploy.Deployment
	max  int
}

func (r *recentDeploys) add(d *deploy.Deployment) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append([]*deploy.Deployment{d}, r.list...)
	if len(r.list) > r.max {
		r.list = r.list[:r.max]
	}
}

func (r *recentDeploys) infos() []deploy.Info {
	r.mu.Lock()
	ds := append([]*deploy.Deployment(nil), r.list...)
	r.mu.Unlock()
	out := make([]deploy.Info, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Info()) // each Info takes that deployment's own lock
	}
	return out
}

type deploysResponse struct {
	Live      string        `json:"live"` // upstream Caddy routes to; "" = nothing
	LiveError string        `json:"live_error,omitempty"`
	Deploys   []deploy.Info `json:"deploys"`
}

// deploysHandler serves GET /deploys: what is live, and the recent deploys
// with their timelines and errors. Error text can include container logs,
// so it needs a bearer token (SLING_STATUS_TOKEN); without one configured
// the endpoint does not exist.
type deploysHandler struct {
	token  []byte
	recent *recentDeploys
	live   func(ctx context.Context) (string, error)
}

func (h *deploysHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if len(h.token) == 0 {
		http.NotFound(w, r)
		return
	}
	if !validBearer(r.Header.Get("Authorization"), h.token) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="sling"`)
		reply(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		reply(w, http.StatusMethodNotAllowed, "use GET")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	resp := deploysResponse{Deploys: h.recent.infos()}
	if live, err := h.live(ctx); err != nil {
		resp.LiveError = err.Error()
	} else {
		resp.Live = live
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

// validBearer compares in constant time. Hashing both sides first makes the
// comparison the same length, so not even the token's length leaks.
func validBearer(header string, token []byte) bool {
	got, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return false
	}
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256(token)
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
