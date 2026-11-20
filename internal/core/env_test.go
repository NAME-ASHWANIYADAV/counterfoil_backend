package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "# comment\n\nCFOIL_TEST_A=hello\nexport CFOIL_TEST_B=\"quoted value\"\nCFOIL_TEST_EXISTING=from-file\nBROKEN LINE\nCFOIL_TEST_EMPTY=\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CFOIL_TEST_EXISTING", "from-env")
	t.Setenv("CFOIL_TEST_A", "")
	os.Unsetenv("CFOIL_TEST_A")
	t.Setenv("CFOIL_TEST_B", "")
	os.Unsetenv("CFOIL_TEST_B")

	n := LoadDotEnv(path)
	if n != 2 {
		t.Fatalf("loaded %d, want 2 (A and B)", n)
	}
	if got := os.Getenv("CFOIL_TEST_A"); got != "hello" {
		t.Fatalf("A = %q", got)
	}
	if got := os.Getenv("CFOIL_TEST_B"); got != "quoted value" {
		t.Fatalf("B = %q", got)
	}
	// Never overwrites what the environment already holds.
	if got := os.Getenv("CFOIL_TEST_EXISTING"); got != "from-env" {
		t.Fatalf("EXISTING overwritten: %q", got)
	}
	// A missing file is fine.
	if n := LoadDotEnv(filepath.Join(dir, "nope.env")); n != 0 {
		t.Fatalf("missing file loaded %d", n)
	}
}
