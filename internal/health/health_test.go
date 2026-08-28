package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{
		LivenessDeadline:      200 * time.Millisecond,
		ReadinessDeadline:     500 * time.Millisecond,
		StatusDeadline:        time.Second,
		StatusCacheTTL:        time.Second,
		ReadinessStartupGrace: 15 * time.Second,
	}
}

// fakeClock returns a controllable time.
type fakeClock struct{ t atomic.Int64 }

func (c *fakeClock) now() time.Time      { return time.Unix(0, c.t.Load()) }
func (c *fakeClock) set(d time.Duration) { c.t.Store(int64(d)) }
func (c *fakeClock) add(d time.Duration) { c.t.Add(int64(d)) }

func okProbe(context.Context) error   { return nil }
func failProbe(context.Context) error { return context.DeadlineExceeded }

func TestLiveNeverProbes(t *testing.T) {
	probed := false
	deps := []Dependency{{Name: "s3.bucket", Type: "s3", Required: true, Probe: func(context.Context) error {
		probed = true
		return nil
	}}}
	h := New(testConfig(), deps, time.Now)

	rec := httptest.NewRecorder()
	h.Live(rec, httptest.NewRequest(http.MethodGet, "/-/health/live", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("live code = %d, want 200", rec.Code)
	}
	if probed {
		t.Fatal("live must not invoke the bucket probe")
	}
}

func TestLiveDraining(t *testing.T) {
	h := New(testConfig(), nil, time.Now)
	h.SetDraining()
	rec := httptest.NewRecorder()
	h.Live(rec, httptest.NewRequest(http.MethodGet, "/-/health/live", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("draining live code = %d, want 503", rec.Code)
	}
}

func TestReadyStartupGrace(t *testing.T) {
	clk := &fakeClock{}
	clk.set(time.Second) // 1s after start, within 15s grace
	deps := []Dependency{{Name: "s3.bucket", Type: "s3", Required: true, Probe: okProbe}}
	h := New(testConfig(), deps, clk.now)
	// start was captured at now()==1s; keep clock the same -> still in grace.

	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/-/health/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("startup grace ready code = %d, want 503", rec.Code)
	}
}

func TestReadyHealthyAfterGrace(t *testing.T) {
	clk := &fakeClock{}
	deps := []Dependency{{Name: "s3.bucket", Type: "s3", Required: true, Probe: okProbe}}
	h := New(testConfig(), deps, clk.now)
	clk.add(20 * time.Second) // past grace

	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/-/health/ready", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready code = %d, want 200", rec.Code)
	}
	var body readyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ready" || !body.Required.AllHealthy {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestReadyDrain(t *testing.T) {
	clk := &fakeClock{}
	deps := []Dependency{{Name: "s3.bucket", Type: "s3", Required: true, Probe: okProbe}}
	h := New(testConfig(), deps, clk.now)
	clk.add(20 * time.Second)
	h.SetDraining()

	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/-/health/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("draining ready code = %d, want 503", rec.Code)
	}
}

func TestReadyFailingDependency(t *testing.T) {
	clk := &fakeClock{}
	deps := []Dependency{{Name: "s3.bucket", Type: "s3", Required: true, Probe: failProbe}}
	h := New(testConfig(), deps, clk.now)
	clk.add(20 * time.Second)

	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/-/health/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing ready code = %d, want 503", rec.Code)
	}
	var body readyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Required.Failed) != 1 || body.Required.Failed[0] != "s3.bucket" {
		t.Fatalf("expected failed [s3.bucket], got %v", body.Required.Failed)
	}
}

func TestStatusShapeHealthy(t *testing.T) {
	clk := &fakeClock{}
	deps := []Dependency{{Name: "s3.bucket", Type: "s3", Required: true, Probe: okProbe}}
	h := New(testConfig(), deps, clk.now)
	clk.add(30 * time.Second)

	rec := httptest.NewRecorder()
	h.Status(rec, httptest.NewRequest(http.MethodGet, "/-/health/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	var body statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "healthy" || body.Incomplete {
		t.Fatalf("unexpected status body: %+v", body)
	}
	if body.Counters.OK != 1 {
		t.Fatalf("counters ok = %d, want 1", body.Counters.OK)
	}
	if len(body.Dependencies) != 1 || body.Dependencies[0].Status != "ok" {
		t.Fatalf("unexpected deps: %+v", body.Dependencies)
	}
	if body.Build.Service == "" {
		t.Fatal("expected build metadata")
	}
}

func TestStatusUnhealthyRequired(t *testing.T) {
	clk := &fakeClock{}
	deps := []Dependency{{Name: "s3.bucket", Type: "s3", Required: true, Probe: func(context.Context) error {
		return context.Canceled
	}}}
	h := New(testConfig(), deps, clk.now)
	clk.add(30 * time.Second)

	rec := httptest.NewRecorder()
	h.Status(rec, httptest.NewRequest(http.MethodGet, "/-/health/status", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unhealthy status code = %d, want 503", rec.Code)
	}
	var body statusResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Status != "unhealthy" || body.Counters.Fail != 1 {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestStatusIncompleteOnDeadline(t *testing.T) {
	cfg := testConfig()
	cfg.StatusDeadline = 20 * time.Millisecond
	deps := []Dependency{{Name: "s3.bucket", Type: "s3", Required: true, Probe: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}}
	h := New(cfg, deps, time.Now)

	rec := httptest.NewRecorder()
	h.Status(rec, httptest.NewRequest(http.MethodGet, "/-/health/status", nil))
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("incomplete status code = %d, want 206", rec.Code)
	}
	var body statusResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !body.Incomplete {
		t.Fatalf("expected incomplete=true, got %+v", body)
	}
	if body.Dependencies[0].Status != "timeout" {
		t.Fatalf("expected timeout dep status, got %q", body.Dependencies[0].Status)
	}
}

func TestStatusCache(t *testing.T) {
	clk := &fakeClock{}
	var calls atomic.Int32
	deps := []Dependency{{Name: "s3.bucket", Type: "s3", Required: true, Probe: func(context.Context) error {
		calls.Add(1)
		return nil
	}}}
	h := New(testConfig(), deps, clk.now)
	clk.add(30 * time.Second)

	req := httptest.NewRequest(http.MethodGet, "/-/health/status", nil)
	h.Status(httptest.NewRecorder(), req)
	h.Status(httptest.NewRecorder(), req) // within 1s TTL -> cached
	if calls.Load() != 1 {
		t.Fatalf("expected 1 probe call (cached), got %d", calls.Load())
	}

	clk.add(2 * time.Second) // expire cache
	h.Status(httptest.NewRecorder(), req)
	if calls.Load() != 2 {
		t.Fatalf("expected 2 probe calls after TTL, got %d", calls.Load())
	}
}
