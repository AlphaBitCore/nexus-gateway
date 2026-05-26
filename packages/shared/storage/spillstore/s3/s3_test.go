package s3

import (
	"context"
	"testing"
)

// TestNew_RequiresBucket pins the constructor's required-field
// validation. Live S3 round-trip tests are intentionally NOT included
// here — they require AWS credentials or a MinIO instance, both of
// which would make `go test ./...` flaky in CI / dev. Operators verify
// the s3 backend end-to-end via the dev-stack README's MinIO quickstart.
func TestNew_RequiresBucket(t *testing.T) {
	_, err := New(context.Background(), Options{})
	if err == nil {
		t.Fatal("expected error when Bucket is empty")
	}
}

func TestBackend_Name(t *testing.T) {
	if BackendName != "s3" {
		t.Errorf("BackendName = %q, want s3", BackendName)
	}
}
