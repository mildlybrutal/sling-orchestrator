package deploy

import (
	"errors"
	"strings"
	"testing"
)

func sha40(c byte) string { return strings.Repeat(string(c), 40) }

// A after B: B is live, A stopped (kept by retirePrevious), C only an image.
func sampleState() State {
	a, b, c := sha40('a'), sha40('b'), sha40('c')
	containers := []string{
		"app-" + b + "\t2026-11-08 10:00:00 +0000 UTC\tUp 5 minutes",
		"app-" + a + "\t2026-11-08 09:00:00 +0000 UTC\tExited (0) 4 minutes ago",
		"sling-caddy\t2026-11-01 00:00:00 +0000 UTC\tUp 7 days", // not an app: ignored
	}
	images := []string{
		b + "\t2026-11-08 09:59:00 +0000 UTC\t6 minutes ago",
		a + "\t2026-11-08 08:59:00 +0000 UTC\t1 hour ago",
		c + "\t2026-11-07 08:00:00 +0000 UTC\t26 hours ago",
		"<none>\t2026-11-07 08:00:00 +0000 UTC\t26 hours ago", // dangling: ignored
	}
	return buildState("app-"+b+":3000", containers, images)
}

func TestBuildState(t *testing.T) {
	st := sampleState()
	if len(st.Versions) != 3 {
		t.Fatalf("want 3 versions, got %d: %+v", len(st.Versions), st.Versions)
	}
	if v := st.Versions[0]; !v.Live || v.SHA != sha40('b') {
		t.Errorf("live version should come first, got %+v", v)
	}
	if st.Versions[1].SHA != sha40('a') || st.Versions[2].SHA != sha40('c') {
		t.Errorf("want newest first after live, got %s then %s", st.Versions[1].SHA[:1], st.Versions[2].SHA[:1])
	}
	if c := st.Versions[2]; c.Container != "" || !c.HasImage {
		t.Errorf("c should be image-only, got %+v", c)
	}
	if st.RollbackTarget != sha40('a') {
		t.Errorf("rollback target = %q, want the stopped previous version a", st.RollbackTarget)
	}
}

func TestRollbackTargetNeedsImageAndContainer(t *testing.T) {
	a, b := sha40('a'), sha40('b')
	// a's container exists but its image is gone: can't recreate it.
	st := buildState("app-"+b+":3000",
		[]string{"app-" + a + "\t2026-11-08 09:00:00 +0000 UTC\tExited (0)"},
		[]string{b + "\t2026-11-08 09:59:00 +0000 UTC\tnow"})
	if st.RollbackTarget != "" {
		t.Errorf("want no target without an image, got %q", st.RollbackTarget)
	}
	// Nothing live, first deploy never happened.
	if st := buildState("", nil, nil); st.RollbackTarget != "" || len(st.Versions) != 0 {
		t.Errorf("empty host: %+v", st)
	}
}

func TestResolveTarget(t *testing.T) {
	st := sampleState()
	cases := []struct {
		arg, want, errHas string
	}{
		{"", sha40('a'), ""},
		{sha40('c'), sha40('c'), ""},      // image only: still allowed by full SHA
		{"cccc", sha40('c'), ""},          // unique prefix
		{"abc", "", "invalid commit"},     // too short
		{"ABCD", "", "invalid commit"},    // uppercase
		{"--rm", "", "invalid commit"},    // never reaches docker
		{"dddd", "", "no image for dddd"}, // unknown
		{sha40('b'), sha40('b'), ""},      // live: release() refuses it with ErrAlreadyLive
	}
	for _, c := range cases {
		got, err := st.resolveTarget(c.arg)
		if c.errHas != "" {
			if err == nil || !strings.Contains(err.Error(), c.errHas) {
				t.Errorf("resolveTarget(%q): want error containing %q, got %q, %v", c.arg, c.errHas, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("resolveTarget(%q) = %q, %v; want %q", c.arg, got, err, c.want)
		}
	}
}

func TestResolveTargetAmbiguousAndNone(t *testing.T) {
	x, y := "abcd"+strings.Repeat("1", 36), "abcd"+strings.Repeat("2", 36)
	st := buildState("", nil, []string{x + "\t1\tnow", y + "\t2\tnow"})
	if _, err := st.resolveTarget("abcd"); err == nil || !strings.Contains(err.Error(), "matches 2") {
		t.Errorf("want ambiguity error, got %v", err)
	}
	if _, err := st.resolveTarget(""); !errors.Is(err, ErrNoRollbackTarget) {
		t.Errorf("want ErrNoRollbackTarget, got %v", err)
	}
}

func TestInfoIsJSONReady(t *testing.T) {
	d := &Deployment{CommitSHA: sha40('a')}
	d.SetStatus(StatusQueued)
	d.setError(errors.New("boom"))
	d.SetStatus(StatusFailed)
	in := d.Info()
	if in.Kind != KindDeploy || in.Status != StatusFailed || in.Error != "boom" || len(in.Timeline) != 2 {
		t.Errorf("Info = %+v", in)
	}
}
