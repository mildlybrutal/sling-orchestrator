package deploy

import (
	"fmt"
	"time"
)

type Status string

const (
	StatusQueued         Status = "queued"
	StatusPreparing      Status = "preparing" // network + proxy container ready
	StatusCloning        Status = "cloning"
	StatusBuilding       Status = "building"
	StatusStarting       Status = "starting"
	StatusHealthChecking Status = "health_checking"
	StatusHealthy        Status = "healthy"   // passed checks, not yet receiving traffic
	StatusSwitching      Status = "switching" // proxy being pointed at the new version
	StatusObserving      Status = "observing" // new version serving, old one kept for rollback
	StatusFailed         Status = "failed"
	StatusLive           Status = "live"
	StatusRolledBack     Status = "rolled_back"
	StatusSuperseded     Status = "superseded" // a newer push replaced it before it started
	StatusSkipped        Status = "skipped"    // that commit was already live
)

type StatusChange struct {
	Status Status    `json:"status"`
	At     time.Time `json:"at"`
}

// Info is a JSON-ready copy of a deployment, safe to build while the
// pipeline is still running (it only reads fields guarded by mu, plus ones
// fixed before the deployment was handed to the pipeline).
type Info struct {
	Kind     string         `json:"kind"`
	Commit   string         `json:"commit"`
	Status   Status         `json:"status"`
	Error    string         `json:"error,omitempty"`
	Timeline []StatusChange `json:"timeline"`
}

func (d *Deployment) Info() Info {
	d.mu.Lock()
	defer d.mu.Unlock()
	kind := d.Kind
	if kind == "" {
		kind = KindDeploy
	}
	h := make([]StatusChange, len(d.history))
	copy(h, d.history)
	return Info{Kind: kind, Commit: d.CommitSHA, Status: d.status, Error: d.err, Timeline: h}
}

func (d *Deployment) setError(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.err = err.Error()
}

func (d *Deployment) SetStatus(s Status) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status = s
	d.history = append(d.history, StatusChange{Status: s, At: time.Now()})
}

// Snapshot returns a copy that is safe to hand to another goroutine.
func (d *Deployment) Snapshot() (Status, []StatusChange) {
	d.mu.Lock()
	defer d.mu.Unlock()
	h := make([]StatusChange, len(d.history))
	copy(h, d.history)
	return d.status, h
}

func (d *Deployment) PrintTimeline() {
	_, h := d.Snapshot()
	if len(h) == 0 {
		return
	}
	fmt.Println("\nTimeline:")
	for i, c := range h {
		var took time.Duration
		if i+1 < len(h) {
			took = h[i+1].At.Sub(c.At)
		}
		fmt.Printf("   %-16s %8s\n", c.Status, took.Round(time.Millisecond))
	}
	fmt.Printf("   total            %8s\n", h[len(h)-1].At.Sub(h[0].At).Round(time.Millisecond))
}
