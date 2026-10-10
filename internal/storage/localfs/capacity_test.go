package localfs

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCapacityReportsVolume(t *testing.T) {
	s := &Storage{root: t.TempDir()}
	c, err := s.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if c.Total <= 0 || c.Free < 0 || c.Free > c.Total {
		t.Fatalf("capacity = %+v, want 0 <= free <= total, total > 0", c)
	}
}

// A root that does not exist yet (Write creates it lazily) measures the volume
// of its nearest existing ancestor rather than failing.
func TestCapacityMissingRootUsesAncestor(t *testing.T) {
	base := t.TempDir()
	want, err := (&Storage{root: base}).Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity(base): %v", err)
	}
	got, err := (&Storage{root: filepath.Join(base, "not", "yet")}).Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity(missing): %v", err)
	}
	if got.Total != want.Total {
		t.Fatalf("total = %d, want %d (same volume as ancestor)", got.Total, want.Total)
	}
}
