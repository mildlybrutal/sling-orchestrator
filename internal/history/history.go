// Package history keeps a durable record of deploys in a SQLite file, so
// "sling history", GET /deploys and crash detection survive restarts.
//
// It uses modernc.org/sqlite, a pure-Go SQLite: no cgo, so the sling binary
// stays a single static file. The CLI and "sling serve" can write to the same
// file at once: WAL mode lets readers and a writer work together, and the
// busy timeout makes a second writer wait instead of failing.
package history

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/mildlybrutal/sling-orchestrator/internal/deploy"
)

// Keep at most this many deploys; older ones are deleted when a store opens.
const keepRows = 1000

// Store is the deploy history. Safe for concurrent use.
type Store struct {
	db *sql.DB
}

// DefaultPath is where history lives: $SLING_DATA_DIR, else
// $XDG_STATE_HOME/sling, else ~/.local/state/sling (the XDG spot for
// "state that should survive a reboot but isn't config").
func DefaultPath() (string, error) {
	dir := os.Getenv("SLING_DATA_DIR")
	if dir == "" {
		state := os.Getenv("XDG_STATE_HOME")
		if state == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("no SLING_DATA_DIR, XDG_STATE_HOME or home directory: %w", err)
			}
			state = filepath.Join(home, ".local", "state")
		}
		dir = filepath.Join(state, "sling")
	}
	return filepath.Join(dir, "history.db"), nil
}

const schema = `
CREATE TABLE IF NOT EXISTS deploys (
	id         TEXT PRIMARY KEY,         -- deploy.Deployment.ID(): sorts by time
	kind       TEXT NOT NULL,            -- deploy | rollback
	commit_sha TEXT NOT NULL,
	repo       TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL,
	error      TEXT NOT NULL DEFAULT '',
	steps      INTEGER NOT NULL,         -- len(timeline): newer saves never lose to older ones
	timeline   TEXT NOT NULL,            -- JSON array of {status, at}
	started_at INTEGER NOT NULL,         -- unix nanoseconds
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS deploys_started ON deploys (started_at);
`

// Open opens (creating if needed) the history at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	// _pragma parameters apply to every connection the pool opens.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("history: create schema in %s: %w", path, err)
	}
	if _, err := db.Exec(`DELETE FROM deploys WHERE id NOT IN
		(SELECT id FROM deploys ORDER BY started_at DESC LIMIT ?)`, keepRows); err != nil {
		db.Close()
		return nil, fmt.Errorf("history: trim: %w", err)
	}
	_ = os.Chmod(path, 0o600) // error text in here can include container logs
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Save inserts or updates one deploy. A save carrying fewer timeline steps
// than what is stored is ignored, so two goroutines saving out of order can
// never move a deploy backwards.
func (s *Store) Save(in deploy.Info) error {
	if in.ID == "" || len(in.Timeline) == 0 {
		return fmt.Errorf("history: deploy has no ID or timeline")
	}
	tl, err := json.Marshal(in.Timeline)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO deploys (id, kind, commit_sha, repo, status, error, steps, timeline, started_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			kind = excluded.kind, commit_sha = excluded.commit_sha, repo = excluded.repo,
			status = excluded.status, error = excluded.error, steps = excluded.steps,
			timeline = excluded.timeline, updated_at = excluded.updated_at
		WHERE excluded.steps >= deploys.steps`,
		in.ID, in.Kind, in.Commit, in.Repo, string(in.Status), in.Error, len(in.Timeline), string(tl),
		in.Timeline[0].At.UnixNano(), in.Timeline[len(in.Timeline)-1].At.UnixNano())
	if err != nil {
		return fmt.Errorf("history: save %s: %w", in.ID, err)
	}
	return nil
}

// Recent returns up to limit deploys, newest first.
func (s *Store) Recent(limit int) ([]deploy.Info, error) {
	rows, err := s.db.Query(`SELECT id, kind, commit_sha, repo, status, error, timeline
		FROM deploys ORDER BY started_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	defer rows.Close()
	out := []deploy.Info{}
	for rows.Next() {
		var in deploy.Info
		var status, tl string
		if err := rows.Scan(&in.ID, &in.Kind, &in.Commit, &in.Repo, &status, &in.Error, &tl); err != nil {
			return nil, fmt.Errorf("history: %w", err)
		}
		in.Status = deploy.Status(status)
		if err := json.Unmarshal([]byte(tl), &in.Timeline); err != nil {
			return nil, fmt.Errorf("history: bad timeline for %s: %w", in.ID, err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// MarkInterrupted sets every in-progress deploy except exceptID to
// interrupted, appending that step to its timeline. The caller must hold the
// deploy lock: only then does "in progress" prove the writer is gone.
func (s *Store) MarkInterrupted(exceptID string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("history: %w", err)
	}
	defer tx.Rollback()

	ph := strings.TrimSuffix(strings.Repeat("?,", len(deploy.InProgress)), ",")
	args := []any{exceptID}
	for _, st := range deploy.InProgress {
		args = append(args, string(st))
	}
	rows, err := tx.Query(`SELECT id, status, timeline FROM deploys WHERE id != ? AND status IN (`+ph+`)`, args...)
	if err != nil {
		return 0, fmt.Errorf("history: %w", err)
	}
	type orphan struct {
		id, status string
		tl         []deploy.StatusChange
	}
	var found []orphan
	for rows.Next() {
		var o orphan
		var tl string
		if err := rows.Scan(&o.id, &o.status, &tl); err != nil {
			rows.Close()
			return 0, fmt.Errorf("history: %w", err)
		}
		_ = json.Unmarshal([]byte(tl), &o.tl) // a broken timeline still gets marked
		found = append(found, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("history: %w", err)
	}

	now := time.Now()
	for _, o := range found {
		o.tl = append(o.tl, deploy.StatusChange{Status: deploy.StatusInterrupted, At: now})
		tl, _ := json.Marshal(o.tl)
		msg := fmt.Sprintf("the Sling process stopped while this deploy was %s", o.status)
		if _, err := tx.Exec(`UPDATE deploys SET status = ?, error = ?, steps = ?, timeline = ?, updated_at = ? WHERE id = ?`,
			string(deploy.StatusInterrupted), msg, len(o.tl), string(tl), now.UnixNano(), o.id); err != nil {
			return 0, fmt.Errorf("history: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("history: %w", err)
	}
	return len(found), nil
}
