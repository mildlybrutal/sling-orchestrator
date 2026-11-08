package history

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mildlybrutal/sling-orchestrator/internal/deploy"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "history.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func info(id string, start time.Time, statuses ...deploy.Status) deploy.Info {
	in := deploy.Info{ID: id, Kind: deploy.KindDeploy, Commit: strings.Repeat("a", 40)}
	for i, st := range statuses {
		in.Timeline = append(in.Timeline, deploy.StatusChange{Status: st, At: start.Add(time.Duration(i) * time.Second)})
		in.Status = st
	}
	return in
}

func TestSaveAndRecent(t *testing.T) {
	s, _ := open(t)
	t0 := time.Now()
	if err := s.Save(info("old", t0, deploy.StatusQueued)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(info("new", t0.Add(time.Minute), deploy.StatusQueued)); err != nil {
		t.Fatal(err)
	}
	// Same deploy, further along: an update, not a new row.
	failed := info("old", t0, deploy.StatusQueued, deploy.StatusPreparing, deploy.StatusFailed)
	failed.Error = "boom"
	if err := s.Save(failed); err != nil {
		t.Fatal(err)
	}

	got, err := s.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "new" || got[1].ID != "old" {
		t.Fatalf("want new, old; got %+v", got)
	}
	if o := got[1]; o.Status != deploy.StatusFailed || o.Error != "boom" || len(o.Timeline) != 3 {
		t.Errorf("updated row: %+v", o)
	}
	if !got[1].Timeline[0].At.Equal(t0) {
		t.Errorf("timestamps must round-trip: %v vs %v", got[1].Timeline[0].At, t0)
	}
	if one, _ := s.Recent(1); len(one) != 1 {
		t.Errorf("limit ignored: %d rows", len(one))
	}
}

// An older snapshot saved after a newer one must not move the deploy back.
func TestSaveNeverGoesBackwards(t *testing.T) {
	s, _ := open(t)
	t0 := time.Now()
	_ = s.Save(info("d", t0, deploy.StatusQueued, deploy.StatusPreparing, deploy.StatusLive))
	_ = s.Save(info("d", t0, deploy.StatusQueued))
	got, _ := s.Recent(1)
	if got[0].Status != deploy.StatusLive {
		t.Fatalf("stale save won: %+v", got[0])
	}
}

func TestMarkInterrupted(t *testing.T) {
	s, _ := open(t)
	t0 := time.Now()
	_ = s.Save(info("crashed", t0, deploy.StatusQueued, deploy.StatusBuilding))
	_ = s.Save(info("waiting", t0, deploy.StatusQueued))                                                   // not in progress
	_ = s.Save(info("done", t0, deploy.StatusQueued, deploy.StatusLive))                                   // finished
	_ = s.Save(info("me", t0, deploy.StatusQueued, deploy.StatusPreparing))                                // the caller
	_ = s.Save(info("observing", t0, deploy.StatusQueued, deploy.StatusSwitching, deploy.StatusObserving)) // crashed late

	n, err := s.MarkInterrupted("me")
	if err != nil || n != 2 {
		t.Fatalf("want 2 marked, got %d, %v", n, err)
	}
	all, _ := s.Recent(10)
	by := map[string]deploy.Info{}
	for _, in := range all {
		by[in.ID] = in
	}
	for id, want := range map[string]deploy.Status{
		"crashed": deploy.StatusInterrupted, "observing": deploy.StatusInterrupted,
		"waiting": deploy.StatusQueued, "done": deploy.StatusLive, "me": deploy.StatusPreparing,
	} {
		if by[id].Status != want {
			t.Errorf("%s: status %s, want %s", id, by[id].Status, want)
		}
	}
	c := by["crashed"]
	if last := c.Timeline[len(c.Timeline)-1]; last.Status != deploy.StatusInterrupted {
		t.Errorf("timeline should end with interrupted: %+v", c.Timeline)
	}
	if !strings.Contains(c.Error, "while this deploy was building") {
		t.Errorf("error = %q", c.Error)
	}
	if n, _ := s.MarkInterrupted("me"); n != 0 {
		t.Errorf("second sweep marked %d", n)
	}
}

// Two stores on one file (the CLI and "sling serve") writing at once.
func TestTwoStoresOneFile(t *testing.T) {
	a, path := open(t)
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	var wg sync.WaitGroup
	t0 := time.Now()
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if err := a.Save(info("a"+string(rune('A'+i)), t0, deploy.StatusQueued)); err != nil {
				t.Error(err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			if err := b.Save(info("b"+string(rune('A'+i)), t0, deploy.StatusQueued)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if got, _ := a.Recent(1000); len(got) != 100 {
		t.Fatalf("want 100 rows, got %d", len(got))
	}
}

// End to end through the deploy package: every status change is saved.
func TestRecorderHook(t *testing.T) {
	s, _ := open(t)
	deploy.SetRecorder(s)
	defer deploy.SetRecorder(nil)

	d := &deploy.Deployment{CommitSHA: strings.Repeat("c", 40)}
	d.SetStatus(deploy.StatusQueued)
	d.SetStatus(deploy.StatusSuperseded)

	got, _ := s.Recent(10)
	if len(got) != 1 || got[0].ID != d.ID() || got[0].Status != deploy.StatusSuperseded || len(got[0].Timeline) != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("SLING_DATA_DIR", "/srv/sling")
	if p, _ := DefaultPath(); p != "/srv/sling/history.db" {
		t.Errorf("SLING_DATA_DIR: %s", p)
	}
	t.Setenv("SLING_DATA_DIR", "")
	t.Setenv("XDG_STATE_HOME", "/x/state")
	if p, _ := DefaultPath(); p != "/x/state/sling/history.db" {
		t.Errorf("XDG_STATE_HOME: %s", p)
	}
}
