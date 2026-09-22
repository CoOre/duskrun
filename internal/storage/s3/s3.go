// Package s3 implements Storage over an S3-compatible object store using a
// streaming multipart Uploader (aws-sdk-go-v2 feature/s3/manager). The Uploader
// is the only way to write a NON-seekable stream (the pg_dump pipe) without a
// temp file: it buffers PartSize-sized chunks and uploads them, so process
// memory is ≈ PartSize×Concurrency, independent of the dump size (roadmap §1).
package s3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/duskrun/duskrun/internal/plugin"
)

func init() {
	plugin.Storages.Register("s3", New)
}

// Config is the s3 storage config (the storage.config JSON blob). Credentials
// come from access_key/secret_key when present (resolved upstream from
// *_ref secrets), else the AWS default chain (env, shared config, IAM role).
type Config struct {
	Bucket         string `json:"bucket"`
	Region         string `json:"region"`
	Endpoint       string `json:"endpoint"`         // custom endpoint (MinIO/etc); empty = AWS
	ForcePathStyle bool   `json:"force_path_style"` // path-style addressing (MinIO)
	Prefix         string `json:"prefix"`           // optional key prefix

	AccessKeyRef string `json:"access_key_ref"`
	SecretKeyRef string `json:"secret_key_ref"`
	AccessKey    string `json:"access_key"` // resolved value (optional)
	SecretKey    string `json:"secret_key"` // resolved value (optional)

	PartSizeMiB int64 `json:"part_size_mib"` // default 8; clamped to >= 8 (S3 min is 5)
	Concurrency int   `json:"concurrency"`   // default 4
}

const (
	defaultRegion      = "us-east-1"
	defaultPartSizeMiB = 8
	minPartSizeMiB     = 8
	defaultConcurrency = 4
	mib                = 1024 * 1024
)

// parseConfig decodes and validates/defaults the config. Pure — no AWS calls.
func parseConfig(raw []byte) (Config, error) {
	var c Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c); err != nil {
			return c, fmt.Errorf("s3: bad config: %w", err)
		}
	}
	if c.Bucket == "" {
		return c, errors.New("s3: bucket is required")
	}
	if c.Region == "" {
		c.Region = defaultRegion
	}
	if c.PartSizeMiB == 0 {
		c.PartSizeMiB = defaultPartSizeMiB
	}
	if c.PartSizeMiB < minPartSizeMiB {
		c.PartSizeMiB = minPartSizeMiB
	}
	if c.Concurrency <= 0 {
		c.Concurrency = defaultConcurrency
	}
	// Both or neither inline key: a lone key is a misconfiguration.
	if (c.AccessKey == "") != (c.SecretKey == "") {
		return c, errors.New("s3: access_key and secret_key must be set together")
	}
	c.Prefix = strings.Trim(c.Prefix, "/")
	return c, nil
}

// objectKey maps a pipeline key to the bucket object key, applying the optional
// prefix. Keys are always forward-slash separated with no leading slash.
func (c Config) objectKey(key string) string {
	key = strings.TrimLeft(key, "/")
	if c.Prefix == "" {
		return key
	}
	return c.Prefix + "/" + key
}

// New builds an s3 Storage. AWS config loading happens here (may read env/shared
// config), so it is only invoked when an s3 storage is actually used.
func New(raw []byte) (plugin.Storage, error) {
	c, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(c.Region)}
	if c.AccessKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, ""),
		))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("s3: load aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
		}
		o.UsePathStyle = c.ForcePathStyle
	})
	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = c.PartSizeMiB * mib
		u.Concurrency = c.Concurrency
	})

	return &Storage{cfg: c, client: client, uploader: uploader}, nil
}

// Storage is an S3-compatible artifact store.
type Storage struct {
	cfg      Config
	client   *s3.Client
	uploader *manager.Uploader
}

// countReader tallies bytes read so Write can report the uploaded size (the
// manager Uploader does not return it for a streamed body).
type countReader struct {
	r io.Reader
	n int64
}

func (cr *countReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.n += int64(n)
	return n, err
}

// Write streams r into the bucket via multipart upload and reports the byte
// count. Checksum is left to the pipeline (io.TeeReader), like localfs.
func (s *Storage) Write(ctx context.Context, key string, r io.Reader) (plugin.ObjectMeta, error) {
	k := s.cfg.objectKey(key)
	cr := &countReader{r: r}
	_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(k),
		Body:   cr,
	})
	if err != nil {
		return plugin.ObjectMeta{}, fmt.Errorf("s3: upload %s: %w", k, err)
	}
	return plugin.ObjectMeta{Key: key, Size: cr.n}, nil
}

// Read fetches an object for download/verify.
func (s *Storage) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(s.cfg.objectKey(key)),
	})
	if err != nil {
		return nil, fmt.Errorf("s3: get %s: %w", key, err)
	}
	return out.Body, nil
}

// List returns objects whose (prefixed) key starts with prefix. The returned
// keys are stripped back to the pipeline key space (prefix removed).
func (s *Storage) List(ctx context.Context, prefix string) ([]plugin.Object, error) {
	full := s.cfg.objectKey(prefix)
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.cfg.Bucket),
		Prefix: aws.String(full),
	})
	var out []plugin.Object
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3: list %s: %w", full, err)
		}
		for _, o := range page.Contents {
			out = append(out, plugin.Object{
				Key:      s.stripPrefix(aws.ToString(o.Key)),
				Size:     aws.ToInt64(o.Size),
				Modified: aws.ToTime(o.LastModified),
			})
		}
	}
	return out, nil
}

// Delete removes one object (used by the Retention Manager).
func (s *Storage) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(s.cfg.objectKey(key)),
	})
	if err != nil {
		return fmt.Errorf("s3: delete %s: %w", key, err)
	}
	return nil
}

// stripPrefix maps a bucket object key back to the pipeline key space.
func (s *Storage) stripPrefix(k string) string {
	if s.cfg.Prefix == "" {
		return k
	}
	return strings.TrimPrefix(k, s.cfg.Prefix+"/")
}
