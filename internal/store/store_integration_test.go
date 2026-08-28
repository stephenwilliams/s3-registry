//go:build integration

package store_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stephenwilliams/s3-registry/internal/index"
	"github.com/stephenwilliams/s3-registry/internal/itest"
	"github.com/stephenwilliams/s3-registry/internal/store"
)

var stack *itest.Stack

func TestMain(m *testing.M) {
	ctx := context.Background()
	s, err := itest.Start(ctx)
	if err != nil {
		panic("start minio: " + err.Error())
	}
	stack = s
	code := m.Run()
	_ = stack.Terminate(ctx)
	os.Exit(code)
}

// TestPreconditionSemantics is the gate for the whole harness: it proves the
// object store honors the S3 conditional-write preconditions the store relies
// on. MinIO is used precisely because LocalStack Community does not enforce
// If-Match, which the stale-ETag assertion below would catch (see the plan's
// risk note).
func TestPreconditionSemantics(t *testing.T) {
	ctx := context.Background()
	bucket := stack.CreateBucket(ctx, t)
	st := stack.NewStore(ctx, t, bucket)

	tool := "condtool"
	idx := index.New(tool)
	idx.Upsert("1.0.0", "linux-amd64", index.Artifact{Key: tool + "/1.0.0/linux-amd64/x", SHA256: "a", Size: 1})

	// If-None-Match:* — first create succeeds.
	if err := st.PutIndex(ctx, tool, idx, ""); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Second create must be rejected: the object now exists.
	if err := st.PutIndex(ctx, tool, idx, ""); !errors.Is(err, store.ErrPreconditionFailed) {
		t.Fatalf("second create err=%v, want ErrPreconditionFailed (If-None-Match fidelity)", err)
	}

	// Capture a live ETag, then let a writer bump it.
	got, etag, err := st.GetIndex(ctx, tool)
	if err != nil {
		t.Fatalf("get for etag: %v", err)
	}
	if etag == "" {
		t.Fatal("expected a non-empty ETag from the object store")
	}
	got.Upsert("1.1.0", "linux-amd64", index.Artifact{Key: tool + "/1.1.0/linux-amd64/x", SHA256: "b", Size: 1})
	if err := st.PutIndex(ctx, tool, got, etag); err != nil {
		t.Fatalf("conditional update with live etag: %v", err)
	}

	// A stale If-Match (the etag captured before the bump) must be rejected.
	got.Upsert("1.2.0", "linux-amd64", index.Artifact{Key: tool + "/1.2.0/linux-amd64/x", SHA256: "c", Size: 1})
	if err := st.PutIndex(ctx, tool, got, etag); !errors.Is(err, store.ErrPreconditionFailed) {
		t.Fatalf("stale If-Match err=%v, want ErrPreconditionFailed", err)
	}
}
