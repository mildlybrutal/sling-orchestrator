package main

import (
	"fmt"
	"time"
)

type Status string

const (
	StatusQueued         Status = "queued"
	StatusCloning        Status = "cloning"
	StatusBuilding       Status = "building"
	StatusStarting       Status = "starting"
	StatusHealthChecking Status = "health_checking"
	StatusHealthy        Status = "healthy" // passed checks, not yet receiving traffic (phase 4 adds "live")
	StatusFailed         Status = "failed"
	StatusLive           Status = "live"
	StatusRolledBack     Status = "rolled_back"
)

type StatusChange struct {
	Status Status
	At     time.Time
}

func (d *Deployment) setStatus(s Status) {
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

func (d *Deployment) printTimeline() {
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
