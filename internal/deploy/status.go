package deploy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
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
	StatusSuperseded     Status = "superseded"  // a newer push replaced it before it started
	StatusSkipped        Status = "skipped"     // that commit was already live
	StatusInterrupted    Status = "interrupted" // its process stopped before it finished
)

// InProgress lists the statuses a deploy can only be in while it holds the
// deploy lock. Finding one of these while WE hold the lock means the process
// that wrote it died mid-deploy.
var InProgress = []Status{
	StatusPreparing, StatusCloning, StatusBuilding, StatusStarting,
	StatusHealthChecking, StatusHealthy, StatusSwitching, StatusObserving,
}

type StatusChange struct {
	Status Status    `json:"status"`
	At     time.Time `json:"at"`
}

// Info is a JSON-ready copy of a deployment, safe to build while the
// pipeline is still running (it only reads fields guarded by mu, plus ones
// fixed before the deployment was handed to the pipeline).
type Info struct {
	ID       string         `json:"id"`
	Kind     string         `json:"kind"`
	Commit   string         `json:"commit"`
	Repo     string         `json:"repo,omitempty"`
	Status   Status         `json:"status"`
	Error    string         `json:"error,omitempty"`
	Timeline []StatusChange `json:"timeline"`
}

// Recorder keeps deploy history somewhere durable. Sling works without one;
// a recorder that fails only causes a warning, never a failed deploy.
type Recorder interface {
	Save(Info) error
	// MarkInterrupted sets every in-progress deploy except the given ID to
	// interrupted. Called while holding the deploy lock.
	MarkInterrupted(exceptID string) (int, error)
}

var (
	recMu    sync.Mutex
	recorder Recorder
)

// SetRecorder makes every status change of every deployment get saved to r
// (nil turns recording off).
func SetRecorder(r Recorder) {
	recMu.Lock()
	defer recMu.Unlock()
	recorder = r
}

func currentRecorder() Recorder {
	recMu.Lock()
	defer recMu.Unlock()
	return recorder
}

func (d *Deployment) Info() Info {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.infoLocked()
}

func (d *Deployment) infoLocked() Info {
	kind := d.Kind
	if kind == "" {
		kind = KindDeploy
	}
	h := make([]StatusChange, len(d.history))
	copy(h, d.history)
	return Info{ID: d.id, Kind: kind, Commit: d.CommitSHA, Repo: d.RepoURL, Status: d.status, Error: d.err, Timeline: h}
}

// ID identifies this deployment in the history. It is assigned on the first
// status change and sorts by time.
func (d *Deployment) ID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.id
}

func newID(t time.Time) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return t.UTC().Format("20060102T150405.000000Z") + "-" + hex.EncodeToString(b)
}

func (d *Deployment) setError(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.err = err.Error()
}

func (d *Deployment) SetStatus(s Status) {
	now := time.Now()
	d.mu.Lock()
	if d.id == "" {
		d.id = newID(now)
	}
	d.status = s
	d.history = append(d.history, StatusChange{Status: s, At: now})
	info := d.infoLocked()
	d.mu.Unlock()
	persist(info)
}

// save writes the deployment's current state to the history without a status
// change: for an error attached after the final status was set (a rollback).
func (d *Deployment) save() {
	if d.ID() == "" {
		return // never recorded, nothing to update
	}
	persist(d.Info())
}

// persist hands info to the recorder. Called outside d.mu: a slow disk must
// not block readers of this deployment.
func persist(info Info) {
	if r := currentRecorder(); r != nil {
		if err := r.Save(info); err != nil {
			fmt.Printf("⚠️  could not save deploy history: %v\n", err)
		}
	}
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
