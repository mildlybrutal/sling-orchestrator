package deploy

import (
	"fmt"
	"net/url"
	"strings"
)

// ValidateRepoURL accepts an https URL on an allowed host, or, only when
// allowLocal is set (CLI deploys), a local path.
func ValidateRepoURL(raw string, allowLocal bool) error {
	if raw == "" || strings.HasPrefix(raw, "-") {
		return fmt.Errorf("invalid repo URL %q", raw)
	}
	if allowLocal && !strings.Contains(raw, "://") {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid repo URL %q: %w", raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("repo URL must use https, got %q", u.Scheme)
	}
	if !allowedRepoHosts[u.Host] {
		return fmt.Errorf("repo host %q is not allowed", u.Host)
	}
	return nil
}

// ValidSHA reports whether s is a full 40-character lowercase commit SHA,
// the only form Sling deploys.
func ValidSHA(s string) bool { return shaPattern.MatchString(s) }
