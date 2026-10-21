package sftp

import (
	"context"
	"testing"
)

func TestSftpProbe(t *testing.T) {
	srv := newTestServer(t)
	if err := newStorage(t, srv, srv.root).Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

// A host that is not listening is exactly what the probe exists to report.
func TestSftpProbeUnreachable(t *testing.T) {
	srv := newTestServer(t)
	st := newStorage(t, srv, srv.root, func(c map[string]any) { c["port"] = 1 })
	if err := st.Probe(context.Background()); err == nil {
		t.Fatal("Probe of a closed port succeeded")
	}
}
