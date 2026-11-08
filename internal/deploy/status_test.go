package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type fakeRecorder struct{ saved []Info }

func (f *fakeRecorder) Save(i Info) error                   { f.saved = append(f.saved, i); return nil }
func (f *fakeRecorder) MarkInterrupted(string) (int, error) { return 0, nil }

// A rollback sets rolled_back before its error exists; the history must still
// end up with the reason, not an empty ERROR column.
func TestRolledBackKeepsErrorInHistory(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	rec := &fakeRecorder{}
	SetRecorder(rec)
	defer SetRecorder(nil)

	d := &Deployment{CommitSHA: strings.Repeat("b", 40)}
	cause := errors.New("3 probes in a row failed during the 30s watch")
	err := locked(d, func(context.Context) error {
		// what rollback() does once the proxy is pointed back
		d.SetStatus(StatusRolledBack)
		return fmt.Errorf("%w; traffic restored to app-a:3000", cause)
	})

	last := rec.saved[len(rec.saved)-1]
	if last.ID != d.ID() || last.Status != StatusRolledBack {
		t.Fatalf("last save: %+v", last)
	}
	if last.Error != err.Error() {
		t.Fatalf("history error = %q, want %q", last.Error, err)
	}
}
