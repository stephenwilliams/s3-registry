//go:build integration

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

	// The CLI resolves its store from these package globals; point them at the
	// container once for the whole run. Each test sets flagBucket to its own.
	flagEndpoint = stack.Endpoint
	flagRegion = stack.Region
	flagPathStyle = true

	code := m.Run()
	_ = stack.Terminate(ctx)
	os.Exit(code)
}

// useBucket creates an isolated bucket, points the CLI at it, and returns a
// store for direct inspection.
func useBucket(ctx context.Context, t *testing.T) *store.Store {
	t.Helper()
	bucket := "s3reg-" + sanitizeName(t.Name())
	if err := stack.CreateBucketNamed(ctx, bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	flagBucket = bucket
	return stack.NewStore(ctx, t, bucket)
}

func sanitizeName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out = append(out, r)
		} else {
			out = append(out, '-')
		}
	}
	return string(out)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func writeFile(t *testing.T, dir, name string, body []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

func objectExists(ctx context.Context, t *testing.T, st *store.Store, key string) bool {
	t.Helper()
	body, _, err := st.GetObject(ctx, key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false
		}
		t.Fatalf("get object %s: %v", key, err)
	}
	_ = body.Close()
	return true
}

func TestPublishSingle(t *testing.T) {
	ctx := context.Background()
	st := useBucket(ctx, t)
	dir := t.TempDir()
	body := []byte("single-artifact-payload")
	file := writeFile(t, dir, "demo.tar.gz", body)

	if err := runPublish(ctx, publishFlags{tool: "demo", version: "1.0.0", os: "linux", arch: "amd64", file: file}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	key := "demo/1.0.0/linux-amd64/demo.tar.gz"
	if !objectExists(ctx, t, st, key) {
		t.Fatalf("object %s missing after publish", key)
	}
	idx, _, err := st.GetIndex(ctx, "demo")
	if err != nil {
		t.Fatalf("get index: %v", err)
	}
	art, ok := findArtifact(idx, "1.0.0", "linux-amd64")
	if !ok {
		t.Fatal("index entry missing")
	}
	if art.SHA256 != sha256Hex(body) || art.Size != int64(len(body)) {
		t.Fatalf("index sha/size wrong: %+v", art)
	}

	out := captureStdout(t, func() {
		if err := runLs(ctx, "demo"); err != nil {
			t.Errorf("ls: %v", err)
		}
	})
	if !strings.Contains(out, "1.0.0") || !strings.Contains(out, "linux-amd64") {
		t.Fatalf("ls output missing entry:\n%s", out)
	}
}

func TestPublishDist(t *testing.T) {
	ctx := context.Background()
	st := useBucket(ctx, t)
	dir := t.TempDir()
	distDir := filepath.Join(dir, "dist")
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		t.Fatal(err)
	}

	archives := []string{
		"demo_1.0.0_linux_amd64.tar.gz",
		"demo_1.0.0_linux_arm64.tar.gz",
		"demo_1.0.0_darwin_amd64.tar.gz",
		"demo_1.0.0_darwin_arm64.tar.gz",
	}
	var sums strings.Builder
	for _, name := range archives {
		body := []byte("payload-of-" + name)
		writeFile(t, distDir, name, body)
		fmt.Fprintf(&sums, "%s  %s\n", sha256Hex(body), name)
	}
	checksums := writeFile(t, dir, "checksums.txt", []byte(sums.String()))

	if err := runPublish(ctx, publishFlags{tool: "demo", version: "1.0.0", dist: distDir, checksums: checksums}); err != nil {
		t.Fatalf("publish dist: %v", err)
	}

	idx, _, err := st.GetIndex(ctx, "demo")
	if err != nil {
		t.Fatalf("get index: %v", err)
	}
	wantOSArch := []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"}
	for _, oa := range wantOSArch {
		art, ok := findArtifact(idx, "1.0.0", oa)
		if !ok {
			t.Fatalf("index missing %s", oa)
		}
		if !objectExists(ctx, t, st, art.Key) {
			t.Fatalf("object missing for %s: %s", oa, art.Key)
		}
	}
}

func TestLsListsTools(t *testing.T) {
	ctx := context.Background()
	useBucket(ctx, t)
	seedOne(ctx, t, "alpha", "1.0.0")
	seedOne(ctx, t, "beta", "2.0.0")

	out := captureStdout(t, func() {
		if err := runLs(ctx, ""); err != nil {
			t.Errorf("ls: %v", err)
		}
	})
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Fatalf("ls all missing tools:\n%s", out)
	}
}

func TestRmOneVersion(t *testing.T) {
	ctx := context.Background()
	st := useBucket(ctx, t)
	file := writeFile(t, t.TempDir(), "demo.tar.gz", []byte("x"))
	for _, v := range []string{"1.0.0", "2.0.0"} {
		if err := runPublish(ctx, publishFlags{tool: "demo", version: v, os: "linux", arch: "amd64", file: file}); err != nil {
			t.Fatalf("publish %s: %v", v, err)
		}
	}

	if err := runRm(ctx, "demo", "1.0.0", "linux", "amd64"); err != nil {
		t.Fatalf("rm: %v", err)
	}

	if objectExists(ctx, t, st, "demo/1.0.0/linux-amd64/demo.tar.gz") {
		t.Fatal("removed object still present")
	}
	if !objectExists(ctx, t, st, "demo/2.0.0/linux-amd64/demo.tar.gz") {
		t.Fatal("other version was deleted")
	}
	idx, _, err := st.GetIndex(ctx, "demo")
	if err != nil {
		t.Fatalf("get index: %v", err)
	}
	if _, ok := findArtifact(idx, "1.0.0", "linux-amd64"); ok {
		t.Fatal("index still lists removed version")
	}
	if _, ok := findArtifact(idx, "2.0.0", "linux-amd64"); !ok {
		t.Fatal("index dropped surviving version")
	}
}

func TestReindexRebuilds(t *testing.T) {
	ctx := context.Background()
	st := useBucket(ctx, t)
	file := writeFile(t, t.TempDir(), "demo.tar.gz", []byte("reindex-body"))
	for _, v := range []string{"1.0.0", "1.5.0"} {
		if err := runPublish(ctx, publishFlags{tool: "demo", version: v, os: "linux", arch: "amd64", file: file}); err != nil {
			t.Fatalf("publish %s: %v", v, err)
		}
	}

	// Corrupt the index by deleting it entirely.
	if err := st.DeleteObject(ctx, "demo/index.json"); err != nil {
		t.Fatalf("delete index: %v", err)
	}
	if _, _, err := st.GetIndex(ctx, "demo"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("index still present: %v", err)
	}

	if err := runReindex(ctx, "demo"); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	idx, _, err := st.GetIndex(ctx, "demo")
	if err != nil {
		t.Fatalf("get rebuilt index: %v", err)
	}
	for _, v := range []string{"1.0.0", "1.5.0"} {
		if _, ok := findArtifact(idx, v, "linux-amd64"); !ok {
			t.Fatalf("rebuilt index missing %s", v)
		}
	}
}

func TestVerifyHappyAndTamper(t *testing.T) {
	ctx := context.Background()
	st := useBucket(ctx, t)
	file := writeFile(t, t.TempDir(), "demo.tar.gz", []byte("trustworthy-bytes"))
	if err := runPublish(ctx, publishFlags{tool: "demo", version: "1.0.0", os: "linux", arch: "amd64", file: file}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if err := runVerify(ctx, "demo", ""); err != nil {
		t.Fatalf("verify happy path: %v", err)
	}

	// Tamper the stored object while leaving the index sha256 unchanged.
	key := "demo/1.0.0/linux-amd64/demo.tar.gz"
	if err := st.PutArtifact(ctx, key, strings.NewReader("tampered-bytes")); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	var out string
	err := runVerifyCapturing(t, ctx, "demo", &out)
	if err == nil {
		t.Fatal("verify passed on tampered artifact; want error")
	}
	// Assert it failed for the right reason (a sha mismatch), not e.g. a read
	// error, and that it names the offending version.
	if !strings.Contains(out, "MISMATCH") || !strings.Contains(out, "1.0.0") {
		t.Fatalf("verify output should report a MISMATCH for 1.0.0:\n%s", out)
	}
}

// runVerifyCapturing runs runVerify while capturing its stdout so the test can
// assert the MISMATCH line names the offending version.
func runVerifyCapturing(t *testing.T, ctx context.Context, tool string, out *string) error {
	t.Helper()
	var verifyErr error
	*out = captureStdout(t, func() { verifyErr = runVerify(ctx, tool, "") })
	return verifyErr
}

func seedOne(ctx context.Context, t *testing.T, tool, version string) {
	t.Helper()
	file := writeFile(t, t.TempDir(), "demo.tar.gz", []byte(tool+version))
	if err := runPublish(ctx, publishFlags{tool: tool, version: version, os: "linux", arch: "amd64", file: file}); err != nil {
		t.Fatalf("seed %s: %v", tool, err)
	}
}

func findArtifact(idx *index.Index, version, osArch string) (index.Artifact, bool) {
	for _, v := range idx.Versions {
		if v.Version == version {
			a, ok := v.Artifacts[osArch]
			return a, ok
		}
	}
	return index.Artifact{}, false
}
