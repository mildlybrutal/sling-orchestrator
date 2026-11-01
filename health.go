package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

type healthConfig struct {
	Interval       time.Duration
	Deadline       time.Duration
	RequestTimeout time.Duration
	RequiredStreak int
}

var defaultHealthConfig = healthConfig{
	Interval:       500 * time.Millisecond,
	Deadline:       30 * time.Second,
	RequestTimeout: 2 * time.Second,
	RequiredStreak: 3,
}

// probeResult is what one GET returned.
type probeResult struct {
	Code   int
	Header http.Header
}

// check judges one probe. It returns whether the probe passed and a short
// description of the result, used in error messages either way.
type check func(r probeResult) (ok bool, problem string)

// is200 passes on HTTP 200.
func is200(r probeResult) (bool, string) {
	if r.Code != http.StatusOK {
		return false, fmt.Sprintf("HTTP %d", r.Code)
	}
	return true, "HTTP 200"
}

// servedBy passes on HTTP 200 answered by the given upstream, as reported by
// the proxy in upstreamHeader. It proves the NEW version answered, not just
// that something healthy did.
func servedBy(upstream string) check {
	return func(r probeResult) (bool, string) {
		if ok, problem := is200(r); !ok {
			return false, problem
		}
		got := r.Header.Get(upstreamHeader)
		if got != upstream {
			return false, fmt.Sprintf("HTTP 200 but served by %q, want %q", got, upstream)
		}
		return true, "HTTP 200 from " + upstream
	}
}

// waitHealthy waits until url returns HTTP 200 RequiredStreak times in a row.
func waitHealthy(ctx context.Context, cfg healthConfig, url string, isRunning func() (bool, error)) error {
	return waitUntil(ctx, cfg, url, is200, isRunning)
}

// waitUntil probes url every Interval until pass accepts RequiredStreak
// probes in a row. Any rejected probe resets the streak. It fails at the
// Deadline, or at once if the container stops running.
func waitUntil(ctx context.Context, cfg healthConfig, url string, pass check, isRunning func() (bool, error)) error {
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

func probe(ctx context.Context, client *http.Client, url string) (probeResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return probeResult{}, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return probeResult{}, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return probeResult{Code: resp.StatusCode, Header: resp.Header}, nil
}
