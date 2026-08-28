// Package store wraps the S3 operations the registry needs. Index reads/writes,
// artifact uploads, presigned downloads, tool enumeration, and a bucket probe.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/stephenwilliams/s3-registry/internal/index"
)

// ErrPreconditionFailed is returned by PutIndex when the conditional write
// fails because the object changed since it was read (HTTP 412).
var ErrPreconditionFailed = errors.New("precondition failed: index changed since read")

// ErrNotFound is returned when an object or index does not exist.
var ErrNotFound = errors.New("not found")

// Config configures the store.
type Config struct {
	Bucket string
	Region string
}

// S3API is the subset of the S3 client the store uses. It exists so tests can
// inject a fake.
type S3API interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	HeadBucket(ctx context.Context, in *s3.HeadBucketInput, opts ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
}

// Store performs the registry's S3 operations.
type Store struct {
	cfg   Config
	api   S3API
	presn presignFn
}

// presignFn abstracts URL presigning so tests can inject one.
type presignFn func(ctx context.Context, key string, ttl time.Duration) (string, error)

// New builds a Store from ambient AWS config (env, shared config, instance
// role). Region falls back to the default provider chain when empty.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("bucket is required")
	}
	opts := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg)
	presign := s3.NewPresignClient(client)
	return &Store{
		cfg: cfg,
		api: client,
		presn: func(ctx context.Context, key string, ttl time.Duration) (string, error) {
			req, err := presign.PresignGetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String(cfg.Bucket),
				Key:    aws.String(key),
			}, s3.WithPresignExpires(ttl))
			if err != nil {
				return "", err
			}
			return req.URL, nil
		},
	}, nil
}

// NewWithAPI builds a Store from an injected S3 API and presign func. For tests.
func NewWithAPI(cfg Config, api S3API, presign presignFn) *Store {
	return &Store{cfg: cfg, api: api, presn: presign}
}

func indexKey(tool string) string { return tool + "/index.json" }

// GetIndex reads and parses a tool's index.json, returning its ETag.
func (s *Store) GetIndex(ctx context.Context, tool string) (*index.Index, string, error) {
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(indexKey(tool)),
	})
	if err != nil {
		if isNoSuchKey(err) {
			return nil, "", ErrNotFound
		}
		return nil, "", fmt.Errorf("get index: %w", err)
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read index: %w", err)
	}
	idx, err := index.Load(data)
	if err != nil {
		return nil, "", err
	}
	etag := ""
	if out.ETag != nil {
		etag = *out.ETag
	}
	return idx, etag, nil
}

// PutIndex writes a tool's index.json. When ifMatchETag is non-empty the write
// is conditional and returns ErrPreconditionFailed on a 412. An empty ETag
// requires the object not to exist yet (If-None-Match: *).
func (s *Store) PutIndex(ctx context.Context, tool string, idx *index.Index, ifMatchETag string) error {
	data, err := idx.Marshal()
	if err != nil {
		return err
	}
	in := &s3.PutObjectInput{
		Bucket:      aws.String(s.cfg.Bucket),
		Key:         aws.String(indexKey(tool)),
		Body:        strings.NewReader(string(data)),
		ContentType: aws.String("application/json"),
	}
	if ifMatchETag != "" {
		in.IfMatch = aws.String(ifMatchETag)
	} else {
		in.IfNoneMatch = aws.String("*")
	}
	if _, err := s.api.PutObject(ctx, in); err != nil {
		if isPreconditionFailed(err) {
			return ErrPreconditionFailed
		}
		return fmt.Errorf("put index: %w", err)
	}
	return nil
}

// PutArtifact uploads an artifact object at the given key.
func (s *Store) PutArtifact(ctx context.Context, key string, r io.Reader) error {
	if _, err := s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
		Body:   r,
	}); err != nil {
		return fmt.Errorf("put artifact: %w", err)
	}
	return nil
}

// DeleteObject removes an object.
func (s *Store) DeleteObject(ctx context.Context, key string) error {
	if _, err := s.api.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("delete object: %w", err)
	}
	return nil
}

// PresignGet returns a presigned GET URL valid for ttl.
func (s *Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return s.presn(ctx, key, ttl)
}

// ListTools returns the top-level prefixes (tool names) in the bucket.
func (s *Store) ListTools(ctx context.Context) ([]string, error) {
	var tools []string
	var token *string
	for {
		out, err := s.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.cfg.Bucket),
			Delimiter:         aws.String("/"),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("list tools: %w", err)
		}
		for _, p := range out.CommonPrefixes {
			if p.Prefix == nil {
				continue
			}
			tools = append(tools, strings.TrimSuffix(*p.Prefix, "/"))
		}
		if out.IsTruncated != nil && *out.IsTruncated {
			token = out.NextContinuationToken
			continue
		}
		break
	}
	return tools, nil
}

// ListKeys returns all object keys under a prefix.
func (s *Store) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	var token *string
	for {
		out, err := s.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.cfg.Bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("list keys: %w", err)
		}
		for _, o := range out.Contents {
			if o.Key != nil {
				keys = append(keys, *o.Key)
			}
		}
		if out.IsTruncated != nil && *out.IsTruncated {
			token = out.NextContinuationToken
			continue
		}
		break
	}
	return keys, nil
}

// GetObject streams an object's body. The caller must close it.
func (s *Store) GetObject(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNoSuchKey(err) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, fmt.Errorf("get object: %w", err)
	}
	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return out.Body, size, nil
}

// HeadBucket probes the bucket. Used by the readiness/status health checks.
func (s *Store) HeadBucket(ctx context.Context) error {
	if _, err := s.api.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(s.cfg.Bucket),
	}); err != nil {
		return fmt.Errorf("head bucket: %w", err)
	}
	return nil
}

func isNoSuchKey(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}

func isPreconditionFailed(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "PreconditionFailed", "412":
			return true
		}
	}
	return strings.Contains(err.Error(), "PreconditionFailed")
}
