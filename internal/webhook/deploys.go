package webhook

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mildlybrutal/sling-orchestrator/internal/deploy"
)

// How many recent deploys GET /deploys returns.
const recentDeploysShown = 20

type deploysResponse struct {
	Live      string        `json:"live"` // upstream Caddy routes to; "" = nothing
	LiveError string        `json:"live_error,omitempty"`
	Deploys   []deploy.Info `json:"deploys"`
}

// deploysHandler serves GET /deploys: what is live, and the recent deploys
// (from the history store, so CLI deploys and ones from before a restart
// show up too) with their timelines and errors. Error text can include container logs,
// so it needs a bearer token (SLING_STATUS_TOKEN); without one configured
// the endpoint does not exist.
type deploysHandler struct {
	token  []byte
	recent func(limit int) ([]deploy.Info, error) // newest first
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
	deploys, err := h.recent(recentDeploysShown)
	if err != nil {
		log.Printf("GET /deploys: %v", err)
		reply(w, http.StatusInternalServerError, "could not read deploy history")
		return
	}
	resp := deploysResponse{Deploys: deploys}
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
