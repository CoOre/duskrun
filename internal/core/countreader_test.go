package core

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// TestCountReaderThrottles verifies the reader counts every byte but only reports
// on the interval boundary, and that the total is exact after a full read.
func TestCountReaderThrottles(t *testing.T) {
	var reports []int64
	// Clock advances 100ms per read; interval 200ms → a report every other read.
	cr := &countReader{
		r:        strings.NewReader("0123456789"), // read 1 byte at a time below
		onBytes:  func(total int64) { reports = append(reports, total) },
		now:      clock(time.Unix(0, 0), 100*time.Millisecond),
		interval: 200 * time.Millisecond,
	}

	// Read one byte at a time so each Read triggers a clock tick.
	buf := make([]byte, 1)
	var total int64
	for {
		n, err := cr.Read(buf)
		total += int64(n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}

	if total != 10 || cr.total != 10 {
		t.Fatalf("counted %d/%d bytes, want 10", total, cr.total)
	}
	if len(reports) == 0 {
		t.Fatal("expected at least one throttled report")
	}
	// First read: last is zero → reports immediately (total 1). Then every 200ms.
	if reports[0] != 1 {
		t.Fatalf("first report = %d, want 1 (immediate on first read)", reports[0])
	}
	// Throttling must drop some reports (10 reads, ~half reported).
	if len(reports) >= 10 {
		t.Fatalf("no throttling: %d reports for 10 reads", len(reports))
	}
}
