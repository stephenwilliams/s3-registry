//go:build integration

package itest

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/stephenwilliams/s3-registry/internal/store"
)

// minioImage pins a MinIO release recent enough to enforce S3 conditional
// writes (both If-None-Match:* and If-Match), which the store relies on for
// optimistic concurrency. LocalStack Community does not enforce If-Match, so the
// harness uses MinIO instead (see the plan's risk note).
const minioImage = "minio/minio:RELEASE.2025-04-22T22-12-26Z"

// region is fixed; the value only needs to be consistent between the client and
// the presign step.
const region = "us-east-1"

// Stack is a running MinIO container plus an S3 client wired to it. One Stack is
// shared per test package via TestMain; individual tests create their own
// buckets for isolation.
type Stack struct {
	Endpoint string
	Region   string
	S3       *s3.Client

	ctr *minio.MinioContainer
}

// Start boots MinIO, points AWS credential/region/endpoint env at it (so the
// real store.New picks up the container), and returns a ready Stack.
func Start(ctx context.Context) (*Stack, error) {
	ctr, err := minio.Run(ctx, minioImage)
	if err != nil {
		return nil, fmt.Errorf("start minio: %w", err)
	}
	conn, err := ctr.ConnectionString(ctx)
	if err != nil {
		_ = ctr.Terminate(ctx)
		return nil, fmt.Errorf("minio endpoint: %w", err)
	}
	endpoint := "http://" + conn
	access, secret := ctr.Username, ctr.Password

	// The real store.New loads ambient AWS config; make that resolve to the
	// container with the MinIO root credentials.
	env := map[string]string{
		"AWS_ACCESS_KEY_ID":     access,
		"AWS_SECRET_ACCESS_KEY": secret,
		"AWS_REGION":            region,
		"AWS_ENDPOINT_URL":      endpoint,
	}
	for k, v := range env {
		if err := os.Setenv(k, v); err != nil {
			_ = ctr.Terminate(ctx)
			return nil, fmt.Errorf("set %s: %w", k, err)
		}
	}

	client, err := newS3Client(ctx, endpoint, access, secret)
	if err != nil {
		_ = ctr.Terminate(ctx)
		return nil, err
	}
	return &Stack{Endpoint: endpoint, Region: region, S3: client, ctr: ctr}, nil
}

func newS3Client(ctx context.Context, endpoint, access, secret string) (*s3.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(access, secret, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	}), nil
}

// Terminate stops the container.
func (s *Stack) Terminate(ctx context.Context) error {
	if s.ctr == nil {
		return nil
	}
	return s.ctr.Terminate(ctx)
}

// CreateBucket makes a fresh, empty bucket and fails the test on error.
func (s *Stack) CreateBucket(ctx context.Context, t *testing.T) string {
	t.Helper()
	bucket := fmt.Sprintf("s3reg-%s", sanitize(t.Name()))
	if _, err := s.S3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("create bucket %s: %v", bucket, err)
	}
	return bucket
}

// CreateBucketNamed makes a bucket with an explicit name. For use outside a test
// body (e.g. TestMain), where no *testing.T is available.
func (s *Stack) CreateBucketNamed(ctx context.Context, bucket string) error {
	if _, err := s.S3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		return fmt.Errorf("create bucket %s: %w", bucket, err)
	}
	return nil
}

// NewStore builds the real *store.Store against the given bucket, using the
// endpoint override the prereq added.
func (s *Stack) NewStore(ctx context.Context, t *testing.T, bucket string) *store.Store {
	t.Helper()
	st, err := store.New(ctx, store.Config{
		Bucket:       bucket,
		Region:       region,
		Endpoint:     s.Endpoint,
		UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return st
}

// sanitize lowercases a test name and replaces characters not allowed in an S3
// bucket name so CreateBucket accepts it.
func sanitize(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}
