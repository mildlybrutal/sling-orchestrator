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

func waitHealthy(ctx context.Context, cfg healthConfig, url string, isRunning func() (bool, error)) error {
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

		code, err := probe(ctx, client, url)
		if ctx.Err() != nil {
			return fmt.Errorf("not healthy after %v (last result: %s)", cfg.Deadline, lastProblem)
		}

		switch {
		case err != nil:
			streak = 0
			lastProblem = err.Error()
		case code != http.StatusOK:
			streak = 0
			lastProblem = fmt.Sprintf("HTTP %d", code)
		default:
			streak++
			lastProblem = fmt.Sprintf("HTTP 200 (%d/%d)", streak, cfg.RequiredStreak)
			if streak >= cfg.RequiredStreak {
				return nil
			}
		}
	}
}

func probe(ctx context.Context, client *http.Client, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}
