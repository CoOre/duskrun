package core

import "testing"

// TestCheckToolPresent resolves a tool known to exist (the go toolchain) and
// gets a non-empty version.
func TestCheckToolPresent(t *testing.T) {
	v, err := CheckTool("go")
	if err != nil {
		t.Fatalf("CheckTool(go): %v", err)
	}
	if v == "" {
		t.Fatal("version empty, want non-empty")
	}
}

// TestCheckToolMissing errors for a binary that does not exist.
func TestCheckToolMissing(t *testing.T) {
	if _, err := CheckTool("definitely-not-a-real-binary-duskrun-xyz"); err == nil {
		t.Fatal("CheckTool for a missing binary returned nil, want error")
	}
}

func TestRequiredTool(t *testing.T) {
	cases := map[string]string{"postgres": "pg_dump", "mysql": "mysqldump", "sqlite": ""}
	for engine, want := range cases {
		if got := RequiredTool(engine); got != want {
			t.Errorf("RequiredTool(%q) = %q, want %q", engine, got, want)
		}
	}
}
