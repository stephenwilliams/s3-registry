//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/stephenwilliams/s3-registry/internal/index"
	"github.com/stephenwilliams/s3-registry/internal/store"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// putIndexWithRetry mirrors the CLI conditional read-modify-write loop.
func putIndexWithRetry(ctx context.Context, st *store.Store, tool string, mutate func(*index.Index)) error {
	for attempt := 0; attempt < 8; attempt++ {
		idx, etag, err := st.GetIndex(ctx, tool)
		if errors.Is(err, store.ErrNotFound) {
			idx, etag = index.New(tool), ""
		} else if err != nil {
			return err
		}
		mutate(idx)
		err = st.PutIndex(ctx, tool, idx, etag)
		if errors.Is(err, store.ErrPreconditionFailed) {
			continue
		}
		return err
	}
	return errors.New("index write did not converge")
}

// TestConditionalUpdateRaceConverges runs two writers that each add a distinct
// version under contention; the retry loop must land both.
func TestConditionalUpdateRaceConverges(t *testing.T) {
	ctx := context.Background()
	bucket := stack.CreateBucket(ctx, t)
	st := stack.NewStore(ctx, t, bucket)
	tool := "racetool"

	// Seed so both writers start from a live ETag and truly contend.
	if err := putIndexWithRetry(ctx, st, tool, func(idx *index.Index) {
		idx.Upsert("1.0.0", "linux-amd64", index.Artifact{Key: tool + "/1.0.0/linux-amd64/x", SHA256: "seed", Size: 1})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	versions := []string{"2.0.0", "3.0.0"}
	start := make(chan struct{})
	for i, v := range versions {
		wg.Add(1)
		go func(i int, v string) {
			defer wg.Done()
			<-start
			errs[i] = putIndexWithRetry(ctx, st, tool, func(idx *index.Index) {
				idx.Upsert(v, "linux-amd64", index.Artifact{Key: tool + "/" + v + "/linux-amd64/x", SHA256: v, Size: 1})
			})
		}(i, v)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	idx, _, err := st.GetIndex(ctx, tool)
	if err != nil {
		t.Fatalf("final get: %v", err)
	}
	for _, want := range []string{"1.0.0", "2.0.0", "3.0.0"} {
		if !containsVersion(idx, want) {
			t.Fatalf("version %s lost to contention; have %v", want, idx.VersionStrings())
		}
	}
}

func containsVersion(idx *index.Index, v string) bool {
	for _, s := range idx.VersionStrings() {
		if s == v {
			return true
		}
	}
	return false
}

// TestListToolsDelimiter verifies CommonPrefixes enumeration ignores nested keys.
func TestListToolsDelimiter(t *testing.T) {
	ctx := context.Background()
	bucket := stack.CreateBucket(ctx, t)
	st := stack.NewStore(ctx, t, bucket)

	for _, k := range []string{
		"toolA/1.0.0/linux-amd64/f", "toolA/index.json",
		"toolB/2.0.0/darwin-arm64/f", "toolB/2.0.0/linux-amd64/f",
	} {
		if err := st.PutArtifact(ctx, k, strings.NewReader("x")); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}
	tools, err := st.ListTools(ctx)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	got := map[string]bool{}
	for _, tl := range tools {
		got[tl] = true
	}
	if !got["toolA"] || !got["toolB"] || len(tools) != 2 {
		t.Fatalf("tools=%v, want exactly [toolA toolB]", tools)
	}
}

// TestListKeysPagination forces the ContinuationToken loop by seeding more than
// one S3 page (1000) of keys under a single prefix.
func TestListKeysPagination(t *testing.T) {
	if testing.Short() {
		t.Skip("pagination seeds >1000 objects")
	}
	ctx := context.Background()
	bucket := stack.CreateBucket(ctx, t)
	st := stack.NewStore(ctx, t, bucket)

	const n = 1050
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(24)
	for i := 0; i < n; i++ {
		i := i
		g.Go(func() error {
			return st.PutArtifact(gctx, fmt.Sprintf("pagetool/k%04d", i), strings.NewReader("x"))
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	keys, err := st.ListKeys(ctx, "pagetool/")
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != n {
		t.Fatalf("got %d keys, want %d (pagination lost objects)", len(keys), n)
	}
}

// TestPresignedGetIsReal presigns a GET, fetches it over HTTP, and checks the
// body, size, and that the requested TTL is encoded in the URL.
func TestPresignedGetIsReal(t *testing.T) {
	ctx := context.Background()
	bucket := stack.CreateBucket(ctx, t)
	st := stack.NewStore(ctx, t, bucket)

	body := []byte("hello-artifact-bytes")
	key := "presigntool/1.0.0/linux-amd64/app.tar.gz"
	if err := st.PutArtifact(ctx, key, strings.NewReader(string(body))); err != nil {
		t.Fatalf("put: %v", err)
	}

	ttl := 90 * time.Second
	u, err := st.PresignGet(ctx, key, ttl)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	if exp := parsed.Query().Get("X-Amz-Expires"); exp != fmt.Sprintf("%d", int(ttl.Seconds())) {
		t.Fatalf("X-Amz-Expires=%q, want %d", exp, int(ttl.Seconds()))
	}

	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("http get presigned: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned GET status=%d", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("body=%q, want %q", got, body)
	}
	if sha256Hex(got) != sha256Hex(body) {
		t.Fatal("sha256 of presigned body did not match uploaded object")
	}
}

// TestGetIndexNotFoundReal checks the NoSuchKey -> ErrNotFound mapping on real S3.
func TestGetIndexNotFoundReal(t *testing.T) {
	ctx := context.Background()
	bucket := stack.CreateBucket(ctx, t)
	st := stack.NewStore(ctx, t, bucket)

	if _, _, err := st.GetIndex(ctx, "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err=%v, want store.ErrNotFound", err)
	}
}
