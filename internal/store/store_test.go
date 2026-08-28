package store

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/stephenwilliams/s3-registry/internal/index"
)

// fakeS3 implements S3API for tests.
type fakeS3 struct {
	getObject  func(*s3.GetObjectInput) (*s3.GetObjectOutput, error)
	putObject  func(*s3.PutObjectInput) (*s3.PutObjectOutput, error)
	listV2     func(*s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error)
	headBucket func(*s3.HeadBucketInput) (*s3.HeadBucketOutput, error)
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return f.getObject(in)
}
func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	return f.putObject(in)
}
func (f *fakeS3) DeleteObject(_ context.Context, _ *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	return &s3.DeleteObjectOutput{}, nil
}
func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	return f.listV2(in)
}
func (f *fakeS3) HeadBucket(_ context.Context, in *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	return f.headBucket(in)
}

func noPresign(context.Context, string, time.Duration) (string, error) {
	return "https://example/url", nil
}

func TestGetIndexNotFound(t *testing.T) {
	f := &fakeS3{getObject: func(*s3.GetObjectInput) (*s3.GetObjectOutput, error) {
		return nil, &types.NoSuchKey{}
	}}
	st := NewWithAPI(Config{Bucket: "b"}, f, noPresign)
	if _, _, err := st.GetIndex(context.Background(), "mytool"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetIndexParses(t *testing.T) {
	idx := index.New("mytool")
	idx.Upsert("1.0.0", "linux-amd64", index.Artifact{Key: "mytool/1.0.0/linux-amd64/x", SHA256: "h", Size: 1})
	data, _ := idx.Marshal()
	f := &fakeS3{getObject: func(*s3.GetObjectInput) (*s3.GetObjectOutput, error) {
		return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(data)), ETag: aws.String("\"etag1\"")}, nil
	}}
	st := NewWithAPI(Config{Bucket: "b"}, f, noPresign)
	got, etag, err := st.GetIndex(context.Background(), "mytool")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if etag != "\"etag1\"" || got.Name != "mytool" || len(got.Versions) != 1 {
		t.Fatalf("unexpected index %+v etag %q", got, etag)
	}
}

func TestPutIndexPrecondition(t *testing.T) {
	f := &fakeS3{putObject: func(in *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
		if in.IfMatch == nil || *in.IfMatch != "\"etag1\"" {
			t.Fatalf("expected If-Match etag1, got %v", in.IfMatch)
		}
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "changed"}
	}}
	st := NewWithAPI(Config{Bucket: "b"}, f, noPresign)
	err := st.PutIndex(context.Background(), "mytool", index.New("mytool"), "\"etag1\"")
	if err != ErrPreconditionFailed {
		t.Fatalf("err = %v, want ErrPreconditionFailed", err)
	}
}

func TestPutIndexCreateUsesIfNoneMatch(t *testing.T) {
	f := &fakeS3{putObject: func(in *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
		if in.IfNoneMatch == nil || *in.IfNoneMatch != "*" {
			t.Fatalf("expected If-None-Match *, got %v", in.IfNoneMatch)
		}
		return &s3.PutObjectOutput{}, nil
	}}
	st := NewWithAPI(Config{Bucket: "b"}, f, noPresign)
	if err := st.PutIndex(context.Background(), "mytool", index.New("mytool"), ""); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestListToolsPaginates(t *testing.T) {
	page := 0
	f := &fakeS3{listV2: func(in *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
		page++
		if page == 1 {
			return &s3.ListObjectsV2Output{
				CommonPrefixes:        []types.CommonPrefix{{Prefix: aws.String("alpha/")}, {Prefix: aws.String("beta/")}},
				IsTruncated:           aws.Bool(true),
				NextContinuationToken: aws.String("tok"),
			}, nil
		}
		return &s3.ListObjectsV2Output{
			CommonPrefixes: []types.CommonPrefix{{Prefix: aws.String("gamma/")}},
			IsTruncated:    aws.Bool(false),
		}, nil
	}}
	st := NewWithAPI(Config{Bucket: "b"}, f, noPresign)
	tools, err := st.ListTools(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := []string{"alpha", "beta", "gamma"}
	if len(tools) != 3 || tools[0] != want[0] || tools[2] != want[2] {
		t.Fatalf("tools = %v, want %v", tools, want)
	}
}

func TestPresignGet(t *testing.T) {
	st := NewWithAPI(Config{Bucket: "b"}, &fakeS3{}, noPresign)
	url, err := st.PresignGet(context.Background(), "k", time.Minute)
	if err != nil || url != "https://example/url" {
		t.Fatalf("url=%q err=%v", url, err)
	}
}
