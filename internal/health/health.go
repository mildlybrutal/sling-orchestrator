// Package health probes HTTP endpoints until they become healthy (WaitUntil)
// or while they must stay healthy (Watch).
package health

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Config struct {
	Interval       time.Duration
	Deadline       time.Duration
	RequestTimeout time.Duration
	RequiredStreak int
}

var Default = Config{
	Interval:       500 * time.Millisecond,
	Deadline:       30 * time.Second,
	RequestTimeout: 2 * time.Second,
	RequiredStreak: 3,
}

// Result is what one GET returned.
type Result struct {
	Code   int
	Header http.Header
}

// check judges one probe. It returns whether the probe passed and a short
// description of the result, used in error messages either way.
type Check func(r Result) (ok bool, problem string)

// Is200 passes on HTTP 200.
func Is200(r Result) (bool, string) {
	if r.Code != http.StatusOK {
		return false, fmt.Sprintf("HTTP %d", r.Code)
	}
	return true, "HTTP 200"
}

// ServedBy passes on HTTP 200 answered by the given upstream, as reported by
// the proxy in the given header. It proves the NEW version answered, not just
// that something healthy did.
func ServedBy(header, upstream string) Check {
	return func(r Result) (bool, string) {
		if ok, problem := Is200(r); !ok {
			return false, problem
		}
		got := r.Header.Get(header)
		if got != upstream {
			return false, fmt.Sprintf("HTTP 200 but served by %q, want %q", got, upstream)
		}
		return true, "HTTP 200 from " + upstream
	}
}

// WaitHealthy waits until url returns HTTP 200 RequiredStreak times in a row.
func WaitHealthy(ctx context.Context, cfg Config, url string, isRunning func() (bool, error)) error {
	return WaitUntil(ctx, cfg, url, Is200, isRunning)
}

// WaitUntil probes url every Interval until pass accepts RequiredStreak
// probes in a row. Any rejected probe resets the streak. It fails at the
// Deadline, or at once if the container stops running.
func WaitUntil(ctx context.Context, cfg Config, url string, pass Check, isRunning func() (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, cfg.Deadline)
	defer cancel()

	client := &http.Client{
		Timeout:       cfg.RequestTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
	}

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	streak := 0
	lastProblem := "no probe completed"

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("not healthy after %v (last result: %s)", cfg.Deadline, lastProblem)
		case <-ticker.C:
		}

		running, err := isRunning()
		if err != nil {
			return fmt.Errorf("could not check container state: %w", err)
		}
		if !running {
			return fmt.Errorf("container exited during health check (last result: %s)", lastProblem)
		}

		res, err := probe(ctx, client, url)
		if ctx.Err() != nil {
			return fmt.Errorf("not healthy after %v (last result: %s)", cfg.Deadline, lastProblem)
		}
		if err != nil {
			streak = 0
			lastProblem = err.Error()
			continue
		}

		ok, problem := pass(res)
		if !ok {
			streak = 0
			lastProblem = problem
			continue
		}
		streak++
		lastProblem = fmt.Sprintf("%s (%d/%d)", problem, streak, cfg.RequiredStreak)
		if streak >= cfg.RequiredStreak {
			return nil
		}
	}
}

// Watch probes url every Interval for duration and fails as soon as
// maxFailures probes in a row are rejected, or the container stops running.
// Reaching the end of the window is success; a cancelled caller is not.
func Watch(ctx context.Context, cfg Config, url string, pass Check, isRunning func() (bool, error), duration time.Duration, maxFailures int) error {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	client := newProbeClient(cfg)
	ticker := time.NewTicker(cfg.Interval)

	defer ticker.Stop()

	failures := 0
	for {
		select {
		case <-ctx.Done():
			return parent.Err()
		case <-ticker.C:
		}
		running, err := isRunning()
		if err != nil {
			return fmt.Errorf("could not check container state: %w", err)
		}
		if !running {
			return fmt.Errorf("container exited during the %v watch", duration)
		}

		res, err := probe(ctx, client, url)
		if ctx.Err() != nil {
			return parent.Err()
		}

		var ok bool
		var problem string

		if err != nil {
			problem = err.Error()
		} else {
			ok, problem = pass(res)
		}
		if ok {
			failures = 0
			continue
		}
		failures++
		if failures >= maxFailures {
			return fmt.Errorf("%d probes in a row failed during the %v watch (last result: %s)", failures, duration, problem)
		}

	}
}

func newProbeClient(cfg Config) *http.Client {
	return &http.Client{
		Timeout:       cfg.RequestTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func probe(ctx context.Context, client *http.Client, url string) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{}, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return Result{Code: resp.StatusCode, Header: resp.Header}, nil
}
