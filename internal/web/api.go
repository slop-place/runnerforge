package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/slop-place/runnerforge/internal/metrics"
	"github.com/slop-place/runnerforge/internal/store"
)

// The JSON API exists so runnerforge can be driven as code: by the Terraform
// provider, by the Kubernetes reconciler, or by anything else. It is the same
// data the UI edits, so a pool created here and a pool created by clicking are
// the same pool.
//
// Authentication is a bearer token rather than the browser session, because a
// provider has no browser. Tokens come from the bootstrap config: they grant
// full control, so they are deliberately not editable through the UI they
// control.

// apiTokenHeader carries the bearer token.
const apiTokenHeader = "Authorization"

// errUnauthorized is returned when no valid token is presented.
var errUnauthorized = errors.New("a valid API token is required")

// apiAuth wraps a handler with bearer-token authentication.
//
// With no tokens configured the API is refused outright rather than left open.
// The UI can be ungated on a trusted network because a human is looking at it;
// a machine endpoint with no credential is just an open door.
func (s *Server) apiAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.APITokens) == 0 {
			metrics.APIAuth("unconfigured")
			writeJSONError(w, http.StatusForbidden,
				"no API tokens are configured; set api_tokens in the config to use the API")
			return
		}
		presented := strings.TrimPrefix(r.Header.Get(apiTokenHeader), "Bearer ")
		presented = strings.TrimSpace(presented)
		if !s.cfg.HasAPIToken(presented) {
			metrics.APIAuth("denied")
			writeJSONError(w, http.StatusUnauthorized, errUnauthorized.Error())
			return
		}
		metrics.APIAuth("success")
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	// The value is always something this package constructed, so an encode
	// failure would be a programming error, not a runtime condition.
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "could not encode the response", http.StatusInternalServerError)
	}
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// decode reads a JSON request body.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	// Unknown fields are an error rather than silently ignored, so a typo in a
	// Terraform resource surfaces as a failure instead of a setting that never
	// took effect.
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode request body: %w", err)
	}
	return nil
}

// apiRoutes registers the JSON API.
func (s *Server) apiRoutes(mux *http.ServeMux) {
	get := func(p string, h http.HandlerFunc) { mux.HandleFunc("GET "+p, s.apiAuth(h)) }
	post := func(p string, h http.HandlerFunc) { mux.HandleFunc("POST "+p, s.apiAuth(h)) }
	put := func(p string, h http.HandlerFunc) { mux.HandleFunc("PUT "+p, s.apiAuth(h)) }
	del := func(p string, h http.HandlerFunc) { mux.HandleFunc("DELETE "+p, s.apiAuth(h)) }

	get("/api/v1/clouds", s.apiListClouds)
	post("/api/v1/clouds", s.apiCreateCloud)
	get("/api/v1/clouds/{id}", s.apiGetCloud)
	put("/api/v1/clouds/{id}", s.apiUpdateCloud)
	del("/api/v1/clouds/{id}", s.apiDeleteCloud)

	post("/api/v1/sizes", s.apiCreateSize)
	get("/api/v1/sizes/{id}", s.apiGetSize)
	put("/api/v1/sizes/{id}", s.apiUpdateSize)
	del("/api/v1/sizes/{id}", s.apiDeleteSize)

	post("/api/v1/images", s.apiCreateImage)
	get("/api/v1/images/{id}", s.apiGetImage)
	put("/api/v1/images/{id}", s.apiUpdateImage)
	del("/api/v1/images/{id}", s.apiDeleteImage)

	get("/api/v1/forges", s.apiListForges)
	post("/api/v1/forges", s.apiCreateForge)
	get("/api/v1/forges/{id}", s.apiGetForge)
	put("/api/v1/forges/{id}", s.apiUpdateForge)
	del("/api/v1/forges/{id}", s.apiDeleteForge)

	get("/api/v1/pools", s.apiListPools)
	post("/api/v1/pools", s.apiCreatePool)
	get("/api/v1/pools/{id}", s.apiGetPool)
	put("/api/v1/pools/{id}", s.apiUpdatePool)
	del("/api/v1/pools/{id}", s.apiDeletePool)

	get("/api/v1/instances", s.apiListInstances)
}

// cloudBody is the JSON shape of a cloud.
//
// Settings and secrets arrive in one map, exactly as the forms submit them,
// and are split by the driver's own schema. A caller should not have to know
// which of a driver's settings happen to be stored encrypted.
type cloudBody struct {
	Name     string         `json:"name"`
	Driver   string         `json:"driver"`
	Enabled  *bool          `json:"enabled,omitempty"`
	Settings map[string]any `json:"settings,omitempty"`
}

func (s *Server) apiListClouds(w http.ResponseWriter, r *http.Request) {
	clouds, err := s.db.Clouds(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, clouds)
}

func (s *Server) apiGetCloud(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.CloudByID(r.Context(), pathID(r))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) apiDeleteCloud(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	// Deleting a cloud that pools still use would strand any machine running
	// on it, with nothing left to reap them.
	if n := s.countPools(r, "cloud_id = ?", id); n > 0 {
		writeJSONError(w, http.StatusConflict,
			"this cloud is still used by a pool; delete the pool first")
		return
	}
	if err := s.db.WithContext(r.Context()).Delete(&store.Cloud{}, id).Error; err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// countPools counts pools matching a condition.
func (s *Server) countPools(r *http.Request, cond string, args ...any) int64 {
	var n int64
	s.db.WithContext(r.Context()).Model(&store.Pool{}).Where(cond, args...).Count(&n)
	return n
}

func (s *Server) apiListForges(w http.ResponseWriter, r *http.Request) {
	forges, err := s.db.Forges(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, forges)
}

func (s *Server) apiGetForge(w http.ResponseWriter, r *http.Request) {
	f, err := s.db.ForgeByID(r.Context(), pathID(r))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) apiDeleteForge(w http.ResponseWriter, r *http.Request) {
	if n := s.countPools(r, "forge_id = ?", pathID(r)); n > 0 {
		writeJSONError(w, http.StatusConflict,
			"this forge is still used by a pool; delete the pool first")
		return
	}
	if err := s.db.WithContext(r.Context()).Delete(&store.Forge{}, pathID(r)).Error; err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiListPools(w http.ResponseWriter, r *http.Request) {
	pools, err := s.db.Pools(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pools)
}

func (s *Server) apiGetPool(w http.ResponseWriter, r *http.Request) {
	p, err := s.db.PoolByID(r.Context(), pathID(r))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) apiDeletePool(w http.ResponseWriter, r *http.Request) {
	// A pool with machines still running must not vanish: the reaper finds
	// them by the pool name written on them.
	live, err := s.db.LiveInstances(r.Context(), pathID(r))
	if err == nil && len(live) > 0 {
		writeJSONError(w, http.StatusConflict,
			"machines are still running in this pool; wait for them or destroy them first")
		return
	}
	if err := s.db.WithContext(r.Context()).Delete(&store.Pool{}, pathID(r)).Error; err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiListInstances(w http.ResponseWriter, r *http.Request) {
	insts, err := s.db.RecentInstances(r.Context(), listLimit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, insts)
}
