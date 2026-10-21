package localfs

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestLocalfsProbe(t *testing.T) {
	// The root may not exist before the first backup; its volume still answers.
	cfg, _ := json.Marshal(map[string]string{"root": filepath.Join(t.TempDir(), "not", "yet")})
	st, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.(*Storage).Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}
