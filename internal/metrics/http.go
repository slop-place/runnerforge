package metrics

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Config controls the metrics endpoint.
type Config struct {
	// Enabled publishes the endpoint. On by default: metrics nobody scrapes
	// cost almost nothing, and metrics that have to be turned on are metrics
	// nobody has when they need them.
	Enabled *bool `yaml:"enabled"`

	// Path is where the endpoint lives.
	Path string `yaml:"path"`

	// RequireToken gates the endpoint behind the same bearer tokens the JSON
	// API uses. Off by default, which is the normal arrangement for a scrape
	// target on a private network. The endpoint exposes no credentials, but it
	// does name every pool, cloud and forge and say what they cost, so it
	// should be turned on wherever that matters.
	RequireToken bool `yaml:"require_token"`
}

// Route is where the endpoint lives, defaulting to the conventional path.
func (c Config) Route() string {
	if c.Path == "" {
		return "/metrics"
	}
	if !strings.HasPrefix(c.Path, "/") {
		return "/" + c.Path
	}
	return c.Path
}

// enabled reports whether to publish, defaulting to on.
func (c Config) enabled() bool { return c.Enabled == nil || *c.Enabled }

// Handler builds the scrape endpoint, or nil when metrics are turned off.
//
// Prometheus cannot sign in to an identity provider, so this endpoint is never
// behind the browser session; tokens is what gates it when the operator asks
// for that.
func Handler(cfg Config, tokens []string) http.Handler {
	if !cfg.enabled() {
		return nil
	}
	h := promhttp.HandlerFor(Registry, promhttp.HandlerOpts{
		// A collector that cannot read the database should say so in the
		// response rather than serving a page of confident zeroes.
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
	if !cfg.RequireToken {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, tokens) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="runnerforge metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// authorized checks the bearer token in constant time.
func authorized(r *http.Request, tokens []string) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	for _, want := range tokens {
		if want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
			return true
		}
	}
	return false
}

// Middleware times every request.
//
// The route label is the pattern the router matched, never the path. A path
// carries record ids, and a label that grows a new value per machine would turn
// one dashboard into a memory leak.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		HTTPInFlight(1)
		defer HTTPInFlight(-1)

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)

		HTTPRequest(r.Method, routeOf(r), rec.code, time.Since(start))
	})
}

// routeOf is the matched pattern, with the method prefix removed.
//
// Requests that matched nothing are reported as "other" rather than by their
// path, so a scanner walking random URLs cannot create a series per guess.
func routeOf(r *http.Request) string {
	pattern := r.Pattern
	if pattern == "" {
		return "other"
	}
	if _, rest, ok := strings.Cut(pattern, " "); ok {
		return rest
	}
	return pattern
}

// statusRecorder remembers the status code so it can be a label.
type statusRecorder struct {
	http.ResponseWriter

	code    int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.code, s.written = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.written = true
	n, err := s.ResponseWriter.Write(b)
	if err != nil {
		return n, fmt.Errorf("write response: %w", err)
	}
	return n, nil
}

// Flush forwards to the wrapped writer when it can. Without this a streaming
// handler behind the middleware would buffer.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
