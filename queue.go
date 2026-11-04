package main

import (
	"log"
	"sync"
)

// deployQueue runs deploys one at a time and keeps at most one waiting.
// A newer push replaces the waiting one: if three pushes land during a
// build, deploying the middle one would only waste a build that is about
// to be replaced. The deploy already running is never interrupted.
type deployQueue struct {
	mu      sync.Mutex
	pending *Deployment
	wake    chan struct{} // capacity 1: "there may be work"
	run     func(*Deployment)
}

func newDeployQueue(run func(*Deployment)) *deployQueue {
	return &deployQueue{wake: make(chan struct{}, 1), run: run}
}

func (q *deployQueue) submit(d *Deployment) {
	q.mu.Lock()
	old := q.pending
	if old != nil && old.CommitSHA == d.CommitSHA {
		q.mu.Unlock()
		log.Printf("%s is already queued", d.CommitSHA[:7])
		return
	}
	q.pending = d
	q.mu.Unlock()

	if old != nil {
		old.setStatus(StatusSuperseded)
		log.Printf("%s superseded by %s before it started", old.CommitSHA[:7], d.CommitSHA[:7])
	}
	select {
	case q.wake <- struct{}{}:
	default: // a wake-up is already waiting
	}
}

func (q *deployQueue) take() *Deployment {
	q.mu.Lock()
	defer q.mu.Unlock()
	d := q.pending
	q.pending = nil
	return d
}

// worker runs queued deploys until stop is closed. A deploy in progress is
// always finished first, so stopping never leaves the proxy mid-cutover.
func (q *deployQueue) worker(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-q.wake:
		}
		// select picks at random when both are ready, so a wake-up that
		// raced with shutdown must not start a fresh deploy.
		select {
		case <-stop:
			if left := q.take(); left != nil {
				log.Printf("shutting down: %s was queued and will not deploy", left.CommitSHA[:7])
			}
			return
		default:
		}
		for d := q.take(); d != nil; d = q.take() {
			q.run(d)
			select {
			case <-stop:
				if left := q.take(); left != nil {
					log.Printf("shutting down: %s was queued and will not deploy", left.CommitSHA[:7])
				}
				return
			default:
			}
		}
	}
}
