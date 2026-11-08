package deploy

import (
	"strings"
	"testing"
)

func TestStatusHistoryOrder(t *testing.T) {
	d := &Deployment{}
	d.SetStatus(StatusQueued)
	d.SetStatus(StatusBuilding)
	d.SetStatus(StatusFailed)

	s, h := d.Snapshot()
	if s != StatusFailed || len(h) != 3 || h[0].Status != StatusQueued || h[1].Status != StatusBuilding {
		t.Fatalf("unexpected history: %v %v", s, h)
	}
}

// upstreamServer answers 200 and claims to be served by the given upstream,
// the way Caddy reports it.

func TestPreviousContainer(t *testing.T) {
	sha := strings.Repeat("a", 40)
	cur := "app-" + strings.Repeat("b", 40) + ":3000"
	cases := []struct {
		prev, want string
		ok         bool
	}{
		{"", "", false},  // first deploy
		{cur, "", false}, // same version redeployed
		{"app-" + sha + ":3000", "app-" + sha, true},
		{"app-" + sha + ":9999", "", false}, // not our port
		{"evil; rm -rf /:3000", "", false},  // never reaches docker
		{"app-" + sha[:39] + ":3000", "", false},
	}
	for _, c := range cases {
		got, ok := previousContainer(c.prev, cur)
		if got != c.want || ok != c.ok {
			t.Errorf("previousContainer(%q) = %q, %v; want %q, %v", c.prev, got, ok, c.want, c.ok)
		}
	}
}
