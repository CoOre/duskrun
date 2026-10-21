package s3

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestS3Probe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == "/backups" {
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	probe := func(bucket string) error {
		cfg, _ := json.Marshal(Config{
			Bucket: bucket, Region: "us-east-1", Endpoint: srv.URL, ForcePathStyle: true,
			AccessKey: "k", SecretKey: "s",
		})
		st, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return st.(*Storage).Probe(context.Background())
	}
	if err := probe("backups"); err != nil {
		t.Fatalf("Probe of an existing bucket: %v", err)
	}
	if err := probe("gone"); err == nil {
		t.Fatal("Probe of a missing bucket succeeded")
	}
}
