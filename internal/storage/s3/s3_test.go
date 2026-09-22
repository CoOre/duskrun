package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/duskrun/duskrun/internal/plugin"
)

// TestS3ConfigParse checks defaults and validation of the pure config parser.
func TestS3ConfigParse(t *testing.T) {
	// Minimal valid config: defaults applied.
	c, err := parseConfig([]byte(`{"bucket":"backups"}`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if c.Region != "us-east-1" {
		t.Errorf("region = %q, want us-east-1 default", c.Region)
	}
	if c.PartSizeMiB != 8 {
		t.Errorf("part size = %d, want 8 default", c.PartSizeMiB)
	}
	if c.Concurrency != 4 {
		t.Errorf("concurrency = %d, want 4 default", c.Concurrency)
	}

	// Part size below the floor is clamped up.
	c2, err := parseConfig([]byte(`{"bucket":"b","part_size_mib":1}`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if c2.PartSizeMiB != 8 {
		t.Errorf("clamped part size = %d, want 8", c2.PartSizeMiB)
	}

	// Missing bucket is rejected.
	if _, err := parseConfig([]byte(`{}`)); err == nil {
		t.Error("parseConfig without bucket succeeded, want error")
	}
	// A lone inline key is rejected.
	if _, err := parseConfig([]byte(`{"bucket":"b","access_key":"AK"}`)); err == nil {
		t.Error("parseConfig with lone access_key succeeded, want error")
	}
}

// TestS3KeyMapping checks prefix application and inversion.
func TestS3KeyMapping(t *testing.T) {
	noPrefix := Config{}
	if got := noPrefix.objectKey("task/db/2026_dump.zst"); got != "task/db/2026_dump.zst" {
		t.Errorf("objectKey no-prefix = %q", got)
	}
	if got := noPrefix.objectKey("/leading/slash"); got != "leading/slash" {
		t.Errorf("objectKey leading-slash = %q, want stripped", got)
	}

	pfx := Config{Prefix: "duskrun"}
	if got := pfx.objectKey("task/db/x.dump"); got != "duskrun/task/db/x.dump" {
		t.Errorf("objectKey prefixed = %q", got)
	}

	s := &Storage{cfg: pfx}
	if got := s.stripPrefix("duskrun/task/db/x.dump"); got != "task/db/x.dump" {
		t.Errorf("stripPrefix = %q, want task/db/x.dump", got)
	}
}

// TestS3Conformance is a real Write/Read/List/Delete round-trip against a live
// S3/MinIO, gated on env.
func TestS3Conformance(t *testing.T) {
	endpoint := os.Getenv("DUSKRUN_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set DUSKRUN_S3_ENDPOINT (and bucket/keys) to run the s3 conformance test")
	}
	cfg, _ := json.Marshal(Config{
		Bucket:         env("DUSKRUN_S3_BUCKET", "duskrun-test"),
		Region:         env("DUSKRUN_S3_REGION", "us-east-1"),
		Endpoint:       endpoint,
		ForcePathStyle: true,
		AccessKey:      os.Getenv("DUSKRUN_S3_ACCESS_KEY"),
		SecretKey:      os.Getenv("DUSKRUN_S3_SECRET_KEY"),
		Prefix:         "conformance",
	})
	st, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	key := "t/db/obj.bin"
	payload := bytes.Repeat([]byte("s3-roundtrip\n"), 10000)

	meta, err := st.Write(ctx, key, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if meta.Size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", meta.Size, len(payload))
	}
	rc, err := st.Read(ctx, key)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatal("read payload mismatch")
	}
	objs, err := st.List(ctx, "t/")
	if err != nil || len(objs) == 0 {
		t.Fatalf("List = %v, %v; want >=1 object", objs, err)
	}
	if err := st.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// compile-time guard: *Storage satisfies plugin.Storage.
var _ plugin.Storage = (*Storage)(nil)
