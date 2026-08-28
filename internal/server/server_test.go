package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stephenwilliams/s3-registry/internal/health"
	"github.com/stephenwilliams/s3-registry/internal/index"
	"github.com/stephenwilliams/s3-registry/internal/store"
)

type fakeStore struct {
	idx      *index.Index
	notFound bool
	tools    []string
}

func (f *fakeStore) GetIndex(_ context.Context, _ string) (*index.Index, string, error) {
	if f.notFound {
		return nil, "", store.ErrNotFound
	}
	return f.idx, "etag", nil
}
func (f *fakeStore) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://signed/" + key, nil
}
func (f *fakeStore) ListTools(_ context.Context) ([]string, error) { return f.tools, nil }
func (f *fakeStore) HeadBucket(_ context.Context) error            { return nil }

func newTestServer(fs *fakeStore, token string) *Server {
	cfg := Config{
		APIToken:   token,
		PresignTTL: time.Minute,
		CacheTTL:   time.Minute,
		Health: health.Config{
			LivenessDeadline:      200 * time.Millisecond,
			ReadinessDeadline:     500 * time.Millisecond,
			StatusDeadline:        time.Second,
			StatusCacheTTL:        time.Second,
			ReadinessStartupGrace: 15 * time.Second,
		},
	}
	return New(cfg, fs, time.Now)
}

func sampleIndex() *index.Index {
	idx := index.New("mytool")
	idx.Upsert("1.0.0", "linux-amd64", index.Artifact{Key: "mytool/1.0.0/linux-amd64/x.tar.gz", SHA256: "aaa", Size: 10})
	idx.Upsert("1.2.9", "darwin-arm64", index.Artifact{Key: "mytool/1.2.9/darwin-arm64/x.tar.gz", SHA256: "bbb", Size: 20})
	idx.Upsert("1.2.9", "linux-amd64", index.Artifact{Key: "mytool/1.2.9/linux-amd64/x.tar.gz", SHA256: "ccc", Size: 30})
	return idx
}

func do(s *Server, method, target, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestListTools(t *testing.T) {
	s := newTestServer(&fakeStore{tools: []string{"a", "b"}}, "")
	rec := do(s, http.MethodGet, "/tools", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Tools []string `json:"tools"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Tools) != 2 {
		t.Fatalf("tools=%v", body.Tools)
	}
}

func TestResolveAndArtifact(t *testing.T) {
	s := newTestServer(&fakeStore{idx: sampleIndex()}, "")

	rec := do(s, http.MethodGet, "/tools/mytool/resolve?range=^1.0", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve code=%d body=%s", rec.Code, rec.Body.String())
	}
	var rb struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &rb)
	if rb.Version != "1.2.9" {
		t.Fatalf("resolved=%q, want 1.2.9", rb.Version)
	}

	rec = do(s, http.MethodGet, "/tools/mytool/versions/latest/artifact?os=darwin&arch=arm64", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("artifact code=%d body=%s", rec.Code, rec.Body.String())
	}
	var ab struct {
		URL     string `json:"url"`
		SHA256  string `json:"sha256"`
		Size    int64  `json:"size"`
		Version string `json:"version"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ab)
	if ab.Version != "1.2.9" || ab.SHA256 != "bbb" || ab.Size != 20 {
		t.Fatalf("artifact body=%+v", ab)
	}
	if ab.URL != "https://signed/mytool/1.2.9/darwin-arm64/x.tar.gz" {
		t.Fatalf("url=%q", ab.URL)
	}
}

func TestArtifactMissingOSArch(t *testing.T) {
	s := newTestServer(&fakeStore{idx: sampleIndex()}, "")
	rec := do(s, http.MethodGet, "/tools/mytool/versions/1.0.0/artifact?os=windows&arch=arm64", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", rec.Code)
	}
}

func TestResolveUnsatisfiable(t *testing.T) {
	s := newTestServer(&fakeStore{idx: sampleIndex()}, "")
	rec := do(s, http.MethodGet, "/tools/mytool/resolve?range=%3E%3D9.0.0", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code=%d, want 422", rec.Code)
	}
}

func TestUnknownTool404(t *testing.T) {
	s := newTestServer(&fakeStore{notFound: true}, "")
	rec := do(s, http.MethodGet, "/tools/ghost/versions", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", rec.Code)
	}
}

func TestAuthRequired(t *testing.T) {
	s := newTestServer(&fakeStore{tools: []string{"a"}}, "secret")

	if rec := do(s, http.MethodGet, "/tools", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token code=%d, want 401", rec.Code)
	}
	if rec := do(s, http.MethodGet, "/tools", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token code=%d, want 401", rec.Code)
	}
	if rec := do(s, http.MethodGet, "/tools", "secret"); rec.Code != http.StatusOK {
		t.Fatalf("good token code=%d, want 200", rec.Code)
	}
	// Health stays open without a token.
	if rec := do(s, http.MethodGet, "/-/health/live", ""); rec.Code != http.StatusOK {
		t.Fatalf("health code=%d, want 200", rec.Code)
	}
}

func TestConcurrentReadsNoRace(t *testing.T) {
	s := newTestServer(&fakeStore{idx: sampleIndex()}, "")
	targets := []string{
		"/tools/mytool/versions",
		"/tools/mytool/resolve?range=^1.0",
		"/tools/mytool/versions/latest/artifact?os=linux&arch=amd64",
		"/tools/mytool/versions/1.2.9/artifact?os=darwin&arch=arm64",
	}
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		target := targets[i%len(targets)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := do(s, http.MethodGet, target, "")
			if rec.Code != http.StatusOK {
				t.Errorf("%s code=%d", target, rec.Code)
			}
		}()
	}
	wg.Wait()
}

func TestHealthAliasRedirect(t *testing.T) {
	s := newTestServer(&fakeStore{}, "")
	rec := do(s, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusPermanentRedirect {
		t.Fatalf("code=%d, want 308", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/-/health/live" {
		t.Fatalf("location=%q", loc)
	}
}
