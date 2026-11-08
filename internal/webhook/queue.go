package webhook

import (
	"log"
	"sync"

	"github.com/mildlybrutal/sling-orchestrator/internal/deploy"
)

// deployQueue runs deploys one at a time and keeps at most one waiting.
// A newer push replaces the waiting one: if three pushes land during a
// build, deploying the middle one would only waste a build that is about
// to be replaced. The deploy already running is never interrupted.
type deployQueue struct {
	mu      sync.Mutex
	pending *deploy.Deployment
	wake    chan struct{} // capacity 1: "there may be work"
	run     func(*deploy.Deployment)
}

func newDeployQueue(run func(*deploy.Deployment)) *deployQueue {
	return &deployQueue{wake: make(chan struct{}, 1), run: run}
}

// submit queues d and reports whether it was accepted (false: that commit
// was already waiting).
func (q *deployQueue) submit(d *deploy.Deployment) bool {
	q.mu.Lock()
	old := q.pending
	if old != nil && old.CommitSHA == d.CommitSHA {
		q.mu.Unlock()
		log.Printf("%s is already queued", d.CommitSHA[:7])
		return false
	}
	q.pending = d
	q.mu.Unlock()

	if old != nil {
		old.SetStatus(deploy.StatusSuperseded)
		log.Printf("%s superseded by %s before it started", old.CommitSHA[:7], d.CommitSHA[:7])
	}
	select {
	case q.wake <- struct{}{}:
	default: // a wake-up is already waiting
	}
	return true
}

func (q *deployQueue) take() *deploy.Deployment {
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
