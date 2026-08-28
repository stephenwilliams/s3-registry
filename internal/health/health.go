// Package health provides generic liveness, readiness, and status handlers.
// The only downstream dependency here is object storage, probed via a supplied
// function; a clock is injectable so tests stay deterministic.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"github.com/stephenwilliams/s3-registry/internal/buildinfo"
)

// Deadlines and timing knobs for the health handlers.
type Config struct {
	LivenessDeadline      time.Duration
	ReadinessDeadline     time.Duration
	StatusDeadline        time.Duration
	StatusCacheTTL        time.Duration
	ReadinessStartupGrace time.Duration
}

// Dependency is a probed downstream. Probe returns nil when healthy.
type Dependency struct {
	Name     string
	Type     string
	Required bool
	Probe    func(ctx context.Context) error
}

// Handler serves the health endpoints.
type Handler struct {
	cfg   Config
	deps  []Dependency
	now   func() time.Time
	start time.Time

	draining atomic.Bool

	sf       singleflight.Group
	mu       sync.Mutex
	cached   *statusResponse
	cachedAt time.Time
}

// New builds a Handler. now may be nil (defaults to time.Now).
func New(cfg Config, deps []Dependency, now func() time.Time) *Handler {
	if now == nil {
		now = time.Now
	}
	return &Handler{cfg: cfg, deps: deps, now: now, start: now()}
}

// SetDraining marks the process as draining (readiness returns 503).
func (h *Handler) SetDraining() { h.draining.Store(true) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Live reports process liveness. It never probes dependencies.
func (h *Handler) Live(w http.ResponseWriter, r *http.Request) {
	if h.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "shutting_down"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type readyResponse struct {
	Status    string        `json:"status"`
	CheckedAt string        `json:"checked_at"`
	Required  requiredBlock `json:"required"`
	Notes     string        `json:"notes"`
}

type requiredBlock struct {
	AllHealthy bool     `json:"all_healthy"`
	Failed     []string `json:"failed"`
}

// Ready reports readiness. It returns 503 during the startup grace window and
// while draining; otherwise it probes required dependencies within the
// readiness deadline.
func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	now := h.now()
	checkedAt := now.UTC().Format(time.RFC3339)

	if h.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, readyResponse{
			Status: "unready", CheckedAt: checkedAt,
			Required: requiredBlock{AllHealthy: false, Failed: []string{}},
			Notes:    "draining",
		})
		return
	}
	if now.Sub(h.start) < h.cfg.ReadinessStartupGrace {
		writeJSON(w, http.StatusServiceUnavailable, readyResponse{
			Status: "unready", CheckedAt: checkedAt,
			Required: requiredBlock{AllHealthy: false, Failed: []string{}},
			Notes:    "startup grace",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.ReadinessDeadline)
	defer cancel()

	var failed []string
	for _, d := range h.deps {
		if !d.Required {
			continue
		}
		if err := d.Probe(ctx); err != nil {
			failed = append(failed, d.Name)
		}
	}
	if failed == nil {
		failed = []string{}
	}

	if len(failed) > 0 {
		writeJSON(w, http.StatusServiceUnavailable, readyResponse{
			Status: "unready", CheckedAt: checkedAt,
			Required: requiredBlock{AllHealthy: false, Failed: failed},
			Notes:    "required dependency unhealthy",
		})
		return
	}
	writeJSON(w, http.StatusOK, readyResponse{
		Status: "ready", CheckedAt: checkedAt,
		Required: requiredBlock{AllHealthy: true, Failed: failed},
		Notes:    "",
	})
}

type depResult struct {
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	Required  bool    `json:"required"`
	LatencyMS int64   `json:"latency_ms"`
	Status    string  `json:"status"`
	Error     *string `json:"error"`
}

type counters struct {
	OK      int `json:"ok"`
	Warn    int `json:"warn"`
	Fail    int `json:"fail"`
	Timeout int `json:"timeout"`
	Skipped int `json:"skipped"`
}

type statusResponse struct {
	Status       string         `json:"status"`
	Incomplete   bool           `json:"incomplete"`
	CheckedAt    string         `json:"checked_at"`
	CacheAgeMS   int64          `json:"cache_age_ms"`
	Build        buildinfo.Info `json:"build"`
	Uptime       float64        `json:"uptime"`
	Dependencies []depResult    `json:"dependencies"`
	Counters     counters       `json:"counters"`
}

// Status reports build metadata, dependency results, and counters. Results are
// cached for StatusCacheTTL and coalesced via singleflight; probes run in
// parallel under a hard deadline that, when tripped, yields HTTP 206.
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	now := h.now()

	h.mu.Lock()
	if h.cached != nil && now.Sub(h.cachedAt) < h.cfg.StatusCacheTTL {
		resp := *h.cached
		resp.CacheAgeMS = now.Sub(h.cachedAt).Milliseconds()
		resp.Uptime = now.Sub(h.start).Seconds()
		h.mu.Unlock()
		writeJSON(w, statusCode(&resp), resp)
		return
	}
	h.mu.Unlock()

	v, _, _ := h.sf.Do("status", func() (any, error) {
		resp := h.probeStatus(r.Context())
		h.mu.Lock()
		h.cached = resp
		h.cachedAt = h.now()
		h.mu.Unlock()
		return resp, nil
	})
	resp := *(v.(*statusResponse))
	resp.CacheAgeMS = 0
	resp.Uptime = h.now().Sub(h.start).Seconds()
	writeJSON(w, statusCode(&resp), resp)
}

func statusCode(resp *statusResponse) int {
	if resp.Incomplete {
		return http.StatusPartialContent
	}
	if resp.Status == "unhealthy" {
		return http.StatusServiceUnavailable
	}
	return http.StatusOK
}

func (h *Handler) probeStatus(parent context.Context) *statusResponse {
	now := h.now()
	ctx, cancel := context.WithTimeout(parent, h.cfg.StatusDeadline)
	defer cancel()

	results := make([]depResult, len(h.deps))
	g, gctx := errgroup.WithContext(ctx)
	for i, d := range h.deps {
		i, d := i, d
		g.Go(func() error {
			start := h.now()
			err := d.Probe(gctx)
			res := depResult{
				Name:      d.Name,
				Type:      d.Type,
				Required:  d.Required,
				LatencyMS: h.now().Sub(start).Milliseconds(),
				Status:    "ok",
			}
			if err != nil {
				msg := err.Error()
				res.Error = &msg
				if gctx.Err() == context.DeadlineExceeded {
					res.Status = "timeout"
				} else {
					res.Status = "fail"
				}
			}
			results[i] = res
			return nil
		})
	}
	_ = g.Wait()

	incomplete := ctx.Err() == context.DeadlineExceeded

	var c counters
	overall := "healthy"
	for _, res := range results {
		switch res.Status {
		case "ok":
			c.OK++
		case "warn":
			c.Warn++
		case "timeout":
			c.Timeout++
		case "skipped":
			c.Skipped++
		default:
			c.Fail++
		}
		if res.Status != "ok" && res.Status != "warn" && res.Status != "skipped" {
			if res.Required {
				overall = "unhealthy"
			} else if overall != "unhealthy" {
				overall = "degraded"
			}
		}
	}

	return &statusResponse{
		Status:       overall,
		Incomplete:   incomplete,
		CheckedAt:    now.UTC().Format(time.RFC3339),
		Build:        buildinfo.Get(),
		Uptime:       now.Sub(h.start).Seconds(),
		Dependencies: results,
		Counters:     c,
	}
}
