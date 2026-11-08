package deploy

import "testing"

func TestValidateRepoURLLocalOnlyFromCLI(t *testing.T) {
	if err := ValidateRepoURL("../sling", true); err != nil {
		t.Fatalf("CLI local path should be allowed: %v", err)
	}
	if err := ValidateRepoURL("../sling", false); err == nil {
		t.Fatal("local path must be refused for webhook deploys")
	}
	for _, bad := range []string{"http://github.com/a/b", "https://evil.com/a/b", "file:///etc", "-upload-pack=x"} {
		if err := ValidateRepoURL(bad, false); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	if err := ValidateRepoURL("https://github.com/akash/sling.git", false); err != nil {
		t.Fatalf("github https should pass: %v", err)
	}
}
