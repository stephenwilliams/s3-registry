// Package server wires the chi router, the huma API (typed operations + auto
// OpenAPI), the auth middleware, the health endpoints, and an index cache.
package server

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/stephenwilliams/s3-registry/internal/health"
	"github.com/stephenwilliams/s3-registry/internal/index"
)

// Config configures the HTTP server.
type Config struct {
	APIToken   string
	PresignTTL time.Duration
	CacheTTL   time.Duration
	Health     health.Config
}

// storeAPI is the subset of *store.Store the server depends on.
type storeAPI interface {
	GetIndex(ctx context.Context, tool string) (*index.Index, string, error)
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	ListTools(ctx context.Context) ([]string, error)
	HeadBucket(ctx context.Context) error
}

// Server holds the router, health handler, and shared state.
type Server struct {
	cfg    Config
	store  storeAPI
	cache  *indexCache
	health *health.Handler
	router chi.Router
}

// New builds a Server.
func New(cfg Config, st storeAPI, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	deps := []health.Dependency{{
		Name:     "s3.bucket",
		Type:     "s3",
		Required: true,
		Probe:    st.HeadBucket,
	}}
	s := &Server{
		cfg:    cfg,
		store:  st,
		cache:  newIndexCache(st, cfg.CacheTTL),
		health: health.New(cfg.Health, deps, now),
	}
	s.router = s.buildRouter()
	return s
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler { return s.router }

// Health returns the health handler (for drain on shutdown).
func (s *Server) Health() *health.Handler { return s.health }

func (s *Server) buildRouter() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(s.authMiddleware)

	// Health endpoints (neutral naming).
	r.Get("/-/health/live", s.health.Live)
	r.Get("/-/health/ready", s.health.Ready)
	r.Get("/-/health/status", s.health.Status)

	// Deprecated aliases, kept for one release.
	r.Handle("/healthz", http.RedirectHandler("/-/health/live", http.StatusPermanentRedirect))
	r.Handle("/readyz", http.RedirectHandler("/-/health/ready", http.StatusPermanentRedirect))

	api := humachi.New(r, huma.DefaultConfig("s3reg", "1.0.0"))
	s.registerOperations(api)
	return r
}

// authMiddleware requires a bearer token on /tools/** when a token is
// configured. Health and docs routes are always open.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIToken != "" && strings.HasPrefix(r.URL.Path, "/tools") {
			auth := r.Header.Get("Authorization")
			const prefix = "Bearer "
			ok := strings.HasPrefix(auth, prefix) &&
				subtle.ConstantTimeCompare([]byte(strings.TrimSpace(auth[len(prefix):])), []byte(s.cfg.APIToken)) == 1
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"title":"Unauthorized","status":401}`))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
