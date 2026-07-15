package buildinfo

import "testing"

func TestVersionNonEmpty(t *testing.T) {
	t.Parallel()
	if Version() == "" {
		t.Fatal("Version() returned an empty string")
	}
}

func TestVersionStamped(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })

	version = "v1.2.3"
	if got := Version(); got != "v1.2.3" {
		t.Errorf("Version() with stamped value = %q, want %q", got, "v1.2.3")
	}
}
