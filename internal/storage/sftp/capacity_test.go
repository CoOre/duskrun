package sftp

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSftpCapacityReportsVolume(t *testing.T) {
	srv := newTestServer(t)
	st := newStorage(t, srv, srv.root)
	c, err := st.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if c.Total <= 0 || c.Free < 0 || c.Free > c.Total {
		t.Fatalf("capacity = %+v, want 0 <= free <= total, total > 0", c)
	}
}

// The configured path is created by the first Write, so before that the
// nearest existing ancestor is measured instead of failing.
func TestSftpCapacityMissingPathUsesAncestor(t *testing.T) {
	srv := newTestServer(t)
	want, err := newStorage(t, srv, srv.root).Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity(root): %v", err)
	}
	got, err := newStorage(t, srv, filepath.ToSlash(filepath.Join(srv.root, "not", "yet"))).Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity(missing): %v", err)
	}
	if got.Total != want.Total {
		t.Fatalf("total = %d, want %d (same volume as ancestor)", got.Total, want.Total)
	}
}
