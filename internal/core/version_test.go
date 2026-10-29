package core

import "testing"

func TestBuildVersionPrefersTheBuildTimeVersion(t *testing.T) {
	saved := version
	t.Cleanup(func() { version = saved })

	version = ""
	if got := buildVersion(); got == "" {
		t.Errorf("buildVersion without a build-time version is empty, want the build info or (devel)")
	}
	version = "1.2.3"
	if got := buildVersion(); got != "1.2.3" {
		t.Errorf("buildVersion = %q, want the build-time version 1.2.3", got)
	}
}
