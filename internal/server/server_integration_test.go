//go:build integration

package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stephenwilliams/s3-registry/internal/health"
	"github.com/stephenwilliams/s3-registry/internal/index"
	"github.com/stephenwilliams/s3-registry/internal/itest"
	"github.com/stephenwilliams/s3-registry/internal/server"
	"github.com/stephenwilliams/s3-registry/internal/store"
)

var (
	stack        *itest.Stack
	seeded       *store.Store      // real store over a bucket seeded with the demo matrix
	seededBody   map[string][]byte // artifact key -> uploaded bytes
	demoVersions = []string{"1.0.0", "1.2.0", "1.2.3", "2.0.0"}
	demoOSArch   = []string{"linux-amd64", "darwin-arm64"}
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	s, err := itest.Start(ctx)
	if err != nil {
		panic("start minio: " + err.Error())
	}
	stack = s
	if err := seedDemo(ctx); err != nil {
		_ = stack.Terminate(ctx)
		panic("seed: " + err.Error())
	}
	code := m.Run()
	_ = stack.Terminate(ctx)
	os.Exit(code)
}

func seedDemo(ctx context.Context) error {
	bucket := "s3reg-demo"
	if err := stack.CreateBucketNamed(ctx, bucket); err != nil {
		return err
	}
	st, err := store.New(ctx, store.Config{Bucket: bucket, Region: stack.Region, Endpoint: stack.Endpoint, UsePathStyle: true})
	if err != nil {
		return err
	}
	seeded = st
	seededBody = map[string][]byte{}

	idx := index.New("demo")
	for _, v := range demoVersions {
		for _, oa := range demoOSArch {
			body := []byte(fmt.Sprintf("demo-%s-%s-payload", v, oa))
			key := fmt.Sprintf("demo/%s/%s/demo.tar.gz", v, oa)
			if err := st.PutArtifact(ctx, key, strings.NewReader(string(body))); err != nil {
				return err
			}
			seededBody[key] = body
			sum := sha256.Sum256(body)
			idx.Upsert(v, oa, index.Artifact{Key: key, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))})
		}
	}
	return st.PutIndex(ctx, "demo", idx, "")
}

func healthCfg() health.Config {
	return health.Config{
		LivenessDeadline:      200 * time.Millisecond,
		ReadinessDeadline:     500 * time.Millisecond,
		StatusDeadline:        time.Second,
		StatusCacheTTL:        time.Minute,
		ReadinessStartupGrace: 15 * time.Second,
	}
}

func do(s *server.Server, method, target, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestServerReadPaths exercises the read API against the seeded demo tool.
func TestServerReadPaths(t *testing.T) {
	s := server.New(server.Config{PresignTTL: time.Minute, CacheTTL: time.Minute, Health: healthCfg()}, seeded, nil)

	t.Run("list tools", func(t *testing.T) {
		var body struct {
			Tools []string `json:"tools"`
		}
		decode(t, do(s, http.MethodGet, "/tools", ""), http.StatusOK, &body)
		if !contains(body.Tools, "demo") {
			t.Fatalf("tools=%v, want to contain demo", body.Tools)
		}
	})

	t.Run("versions ascending", func(t *testing.T) {
		var body struct {
			Versions []string `json:"versions"`
		}
		decode(t, do(s, http.MethodGet, "/tools/demo/versions", ""), http.StatusOK, &body)
		want := []string{"1.0.0", "1.2.0", "1.2.3", "2.0.0"}
		if strings.Join(body.Versions, ",") != strings.Join(want, ",") {
			t.Fatalf("versions=%v, want %v", body.Versions, want)
		}
	})

	t.Run("resolve matrix", func(t *testing.T) {
		cases := []struct {
			rng, want string
			code      int
		}{
			{"^1.2", "1.2.3", 200},
			{"~1.2.0", "1.2.3", 200},
			{">=1.0 <2.0", "1.2.3", 200},
			{"latest", "2.0.0", 200},
			{"", "2.0.0", 200},
			{"1.2.0", "1.2.0", 200},
			{"^3", "", 422},
		}
		for _, c := range cases {
			rec := do(s, http.MethodGet, "/tools/demo/resolve?range="+url.QueryEscape(c.rng), "")
			if rec.Code != c.code {
				t.Fatalf("range %q code=%d, want %d (body %s)", c.rng, rec.Code, c.code, rec.Body.String())
			}
			if c.code == 200 {
				var b struct {
					Version string `json:"version"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &b)
				if b.Version != c.want {
					t.Fatalf("range %q resolved=%q, want %q", c.rng, b.Version, c.want)
				}
			}
		}
	})
}

// TestServerArtifact resolves an artifact and downloads the presigned URL.
func TestServerArtifact(t *testing.T) {
	s := server.New(server.Config{PresignTTL: time.Minute, CacheTTL: time.Minute, Health: healthCfg()}, seeded, nil)

	t.Run("valid presigned download", func(t *testing.T) {
		var b struct {
			URL     string `json:"url"`
			SHA256  string `json:"sha256"`
			Size    int64  `json:"size"`
			Version string `json:"version"`
		}
		decode(t, do(s, http.MethodGet, "/tools/demo/versions/latest/artifact?os=linux&arch=amd64", ""), http.StatusOK, &b)
		if b.Version != "2.0.0" {
			t.Fatalf("version=%q, want 2.0.0", b.Version)
		}
		key := "demo/2.0.0/linux-amd64/demo.tar.gz"
		want := seededBody[key]
		if b.Size != int64(len(want)) {
			t.Fatalf("size=%d, want %d", b.Size, len(want))
		}
		resp, err := http.Get(b.URL)
		if err != nil {
			t.Fatalf("get presigned: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		got, _ := io.ReadAll(resp.Body)
		if string(got) != string(want) {
			t.Fatalf("downloaded body=%q, want %q", got, want)
		}
		sum := sha256.Sum256(got)
		if hex.EncodeToString(sum[:]) != b.SHA256 {
			t.Fatal("downloaded sha256 != reported sha256")
		}
	})

	t.Run("error codes", func(t *testing.T) {
		cases := []struct {
			target string
			code   int
		}{
			{"/tools/demo/versions/2.0.0/artifact?arch=amd64", 422},           // missing os
			{"/tools/demo/versions/2.0.0/artifact?os=linux", 422},             // missing arch
			{"/tools/demo/versions/9.9.9/artifact?os=linux&arch=amd64", 404},  // unknown version
			{"/tools/demo/versions/2.0.0/artifact?os=plan9&arch=amd64", 404},  // unknown os-arch
			{"/tools/ghost/versions/1.0.0/artifact?os=linux&arch=amd64", 404}, // unknown tool
		}
		for _, c := range cases {
			if rec := do(s, http.MethodGet, c.target, ""); rec.Code != c.code {
				t.Fatalf("%s code=%d, want %d", c.target, rec.Code, c.code)
			}
		}
	})
}

// TestServerAuth checks bearer enforcement on /tools when a token is configured.
func TestServerAuth(t *testing.T) {
	withToken := server.New(server.Config{APIToken: "secret", PresignTTL: time.Minute, CacheTTL: time.Minute, Health: healthCfg()}, seeded, nil)
	noToken := server.New(server.Config{PresignTTL: time.Minute, CacheTTL: time.Minute, Health: healthCfg()}, seeded, nil)

	if rec := do(withToken, http.MethodGet, "/tools", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer code=%d, want 401", rec.Code)
	}
	if rec := do(withToken, http.MethodGet, "/tools", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer code=%d, want 401", rec.Code)
	}
	if rec := do(withToken, http.MethodGet, "/tools", "secret"); rec.Code != http.StatusOK {
		t.Fatalf("good bearer code=%d, want 200", rec.Code)
	}
	if rec := do(withToken, http.MethodGet, "/-/health/live", ""); rec.Code != http.StatusOK {
		t.Fatalf("health should stay open, code=%d", rec.Code)
	}
	if rec := do(noToken, http.MethodGet, "/tools", ""); rec.Code != http.StatusOK {
		t.Fatalf("token unset should open /tools, code=%d", rec.Code)
	}
}

// TestServerHealth drives the health endpoints with a controllable clock and
// probe decorators.
func TestServerHealth(t *testing.T) {
	t.Run("live and ready lifecycle", func(t *testing.T) {
		clk := &clock{t: time.Unix(1_700_000_000, 0)}
		s := server.New(server.Config{PresignTTL: time.Minute, CacheTTL: time.Minute, Health: healthCfg()}, seeded, clk.now)

		if rec := do(s, http.MethodGet, "/-/health/live", ""); rec.Code != http.StatusOK {
			t.Fatalf("live code=%d, want 200", rec.Code)
		}
		if rec := do(s, http.MethodGet, "/-/health/ready", ""); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("ready during grace code=%d, want 503", rec.Code)
		}
		clk.advance(20 * time.Second) // past ReadinessStartupGrace
		if rec := do(s, http.MethodGet, "/-/health/ready", ""); rec.Code != http.StatusOK {
			t.Fatalf("ready after grace code=%d, want 200", rec.Code)
		}
		s.Health().SetDraining()
		if rec := do(s, http.MethodGet, "/-/health/ready", ""); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("ready while draining code=%d, want 503", rec.Code)
		}
	})

	t.Run("status healthy and cached", func(t *testing.T) {
		s := server.New(server.Config{PresignTTL: time.Minute, CacheTTL: time.Minute, Health: healthCfg()}, seeded, nil)
		var first struct {
			Status     string `json:"status"`
			CheckedAt  string `json:"checked_at"`
			CacheAgeMS int64  `json:"cache_age_ms"`
		}
		decode(t, do(s, http.MethodGet, "/-/health/status", ""), http.StatusOK, &first)
		if first.Status != "healthy" {
			t.Fatalf("status=%q, want healthy", first.Status)
		}
		var second struct {
			CheckedAt  string `json:"checked_at"`
			CacheAgeMS int64  `json:"cache_age_ms"`
		}
		decode(t, do(s, http.MethodGet, "/-/health/status", ""), http.StatusOK, &second)
		if second.CheckedAt != first.CheckedAt {
			t.Fatalf("second status not served from cache: %q vs %q", second.CheckedAt, first.CheckedAt)
		}
		if second.CacheAgeMS < 0 {
			t.Fatalf("cache_age_ms=%d, want >= 0", second.CacheAgeMS)
		}
	})

	t.Run("status 206 when probe exceeds deadline", func(t *testing.T) {
		cfg := healthCfg()
		cfg.StatusDeadline = 50 * time.Millisecond
		cfg.StatusCacheTTL = time.Millisecond
		slow := &slowHead{Store: seeded, delay: time.Second}
		s := server.New(server.Config{PresignTTL: time.Minute, CacheTTL: time.Minute, Health: cfg}, slow, nil)
		if rec := do(s, http.MethodGet, "/-/health/status", ""); rec.Code != http.StatusPartialContent {
			t.Fatalf("slow-probe status code=%d, want 206", rec.Code)
		}
	})

	t.Run("status 503 when required dep down", func(t *testing.T) {
		dead, err := store.New(context.Background(), store.Config{Bucket: "does-not-exist-bucket", Region: stack.Region, Endpoint: stack.Endpoint, UsePathStyle: true})
		if err != nil {
			t.Fatalf("dead store: %v", err)
		}
		cfg := healthCfg()
		cfg.StatusCacheTTL = time.Millisecond
		s := server.New(server.Config{PresignTTL: time.Minute, CacheTTL: time.Minute, Health: cfg}, dead, nil)
		if rec := do(s, http.MethodGet, "/-/health/status", ""); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("dead-dep status code=%d, want 503", rec.Code)
		}
	})
}

// TestDeprecatedRedirects checks the 308 aliases.
func TestDeprecatedRedirects(t *testing.T) {
	s := server.New(server.Config{PresignTTL: time.Minute, CacheTTL: time.Minute, Health: healthCfg()}, seeded, nil)
	for path, loc := range map[string]string{"/healthz": "/-/health/live", "/readyz": "/-/health/ready"} {
		rec := do(s, http.MethodGet, path, "")
		if rec.Code != http.StatusPermanentRedirect {
			t.Fatalf("%s code=%d, want 308", path, rec.Code)
		}
		if got := rec.Header().Get("Location"); got != loc {
			t.Fatalf("%s Location=%q, want %q", path, got, loc)
		}
	}
}

// clock is a manually advanced time source for the health handler.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// slowHead wraps the real store but stalls HeadBucket until the context is
// cancelled, forcing the status handler's per-probe deadline to trip.
type slowHead struct {
	*store.Store
	delay time.Duration
}

func (s *slowHead) HeadBucket(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.delay):
		return nil
	}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, wantCode int, v any) {
	t.Helper()
	if rec.Code != wantCode {
		t.Fatalf("code=%d, want %d (body %s)", rec.Code, wantCode, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
