package s3

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/CoOre/duskrun/internal/plugin"
)

var _ plugin.Prober = (*Storage)(nil)

// Probe asks for the bucket's head: one request that fails on a wrong
// endpoint, bad keys or a missing bucket, and needs only the ListBucket
// permission retention already relies on.
func (s *Storage) Probe(ctx context.Context) error {
	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.cfg.Bucket)}); err != nil {
		return fmt.Errorf("s3: probe %s: %w", s.cfg.Bucket, err)
	}
	return nil
}
