package web

import (
	"maps"
	"net/http"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/forge"
	"github.com/slop-place/runnerforge/internal/store"
)

// splitSettings divides a caller's flat settings map into plain settings and
// encrypted credentials, using the driver's own declaration of which is which.
//
// A caller should not have to know that a password happens to live in a
// different column; it declares what it wants configured and runnerforge puts
// each value where it belongs.
func splitSettings(fields []cloud.Field, in map[string]any, existing store.Secret) (store.Params, store.Secret) {
	secret := map[string]bool{}
	for _, f := range fields {
		if f.Secret {
			secret[f.Key] = true
		}
	}
	settings := store.Params{}
	creds := store.Secret{}
	maps.Copy(creds, existing)
	for k, v := range in {
		if !secret[k] {
			settings[k] = v
			continue
		}
		s, ok := v.(string)
		if !ok || s == "" {
			// An omitted or blank secret keeps whatever is stored, matching how
			// the forms behave, so an update that does not mention a password
			// does not erase it.
			continue
		}
		creds[k] = s
	}
	return settings, creds
}

func (s *Server) apiCreateCloud(w http.ResponseWriter, r *http.Request) {
	var body cloudBody
	if err := decode(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	drv, ok := cloud.DriverByName(body.Driver)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "unknown driver "+body.Driver)
		return
	}
	settings, creds := splitSettings(drv.Schema.Connection, body.Settings, nil)
	c := &store.Cloud{
		Name: body.Name, Driver: body.Driver,
		Enabled: boolOr(body.Enabled, true), Settings: settings, Credentials: creds,
	}
	if c.Name == "" {
		writeJSONError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := s.db.WithContext(r.Context()).Create(c).Error; err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) apiUpdateCloud(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.CloudByID(r.Context(), pathID(r))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	var body cloudBody
	if err := decode(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	drv, ok := cloud.DriverByName(c.Driver)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "unknown driver "+c.Driver)
		return
	}
	if body.Name != "" {
		c.Name = body.Name
	}
	c.Enabled = boolOr(body.Enabled, c.Enabled)
	c.Settings, c.Credentials = splitSettings(drv.Schema.Connection, body.Settings, c.Credentials)
	if err := s.db.WithContext(r.Context()).Save(c).Error; err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// sizeBody is the JSON shape of an entry in a cloud's size catalogue.
type sizeBody struct {
	CloudID   uint           `json:"cloud_id"`
	Name      string         `json:"name"`
	Spec      map[string]any `json:"spec,omitempty"`
	VCPUs     int            `json:"vcpus,omitempty"`
	MemoryMB  int            `json:"memory_mb,omitempty"`
	DiskGB    int            `json:"disk_gb,omitempty"`
	HourlyUSD float64        `json:"hourly_usd,omitempty"`
}

func (s *Server) apiCreateSize(w http.ResponseWriter, r *http.Request) {
	var body sizeBody
	if err := decode(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	sz := &store.Size{
		CloudID: body.CloudID, Name: body.Name, Spec: store.Params(body.Spec),
		VCPUs: body.VCPUs, MemoryMB: body.MemoryMB, DiskGB: body.DiskGB,
		HourlyUSD: body.HourlyUSD,
	}
	if err := s.db.WithContext(r.Context()).Create(sz).Error; err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sz)
}

func (s *Server) apiGetSize(w http.ResponseWriter, r *http.Request) {
	var sz store.Size
	if err := s.db.WithContext(r.Context()).First(&sz, pathID(r)).Error; err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sz)
}

func (s *Server) apiUpdateSize(w http.ResponseWriter, r *http.Request) {
	var sz store.Size
	if err := s.db.WithContext(r.Context()).First(&sz, pathID(r)).Error; err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	var body sizeBody
	if err := decode(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Name != "" {
		sz.Name = body.Name
	}
	if body.Spec != nil {
		sz.Spec = store.Params(body.Spec)
	}
	sz.VCPUs, sz.MemoryMB, sz.DiskGB, sz.HourlyUSD = body.VCPUs, body.MemoryMB, body.DiskGB, body.HourlyUSD
	if err := s.db.WithContext(r.Context()).Save(&sz).Error; err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sz)
}

func (s *Server) apiDeleteSize(w http.ResponseWriter, r *http.Request) {
	if n := s.countPools(r, "size_id = ?", pathID(r)); n > 0 {
		writeJSONError(w, http.StatusConflict,
			"this size is still used by a pool; delete the pool first")
		return
	}
	if err := s.db.WithContext(r.Context()).Delete(&store.Size{}, pathID(r)).Error; err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// imageBody is the JSON shape of an entry in a cloud's image catalogue.
type imageBody struct {
	CloudID            uint           `json:"cloud_id"`
	Name               string         `json:"name"`
	Spec               map[string]any `json:"spec,omitempty"`
	Username           string         `json:"username,omitempty"`
	PreinstalledDocker bool           `json:"preinstalled_docker,omitempty"`
	Notes              string         `json:"notes,omitempty"`
}

func (s *Server) apiCreateImage(w http.ResponseWriter, r *http.Request) {
	var body imageBody
	if err := decode(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	img := &store.Image{
		CloudID: body.CloudID, Name: body.Name, Spec: store.Params(body.Spec),
		Username: body.Username, PreinstalledDocker: body.PreinstalledDocker, Notes: body.Notes,
	}
	if err := s.db.WithContext(r.Context()).Create(img).Error; err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, img)
}

func (s *Server) apiGetImage(w http.ResponseWriter, r *http.Request) {
	var img store.Image
	if err := s.db.WithContext(r.Context()).First(&img, pathID(r)).Error; err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, img)
}

func (s *Server) apiUpdateImage(w http.ResponseWriter, r *http.Request) {
	var img store.Image
	if err := s.db.WithContext(r.Context()).First(&img, pathID(r)).Error; err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	var body imageBody
	if err := decode(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Name != "" {
		img.Name = body.Name
	}
	if body.Spec != nil {
		img.Spec = store.Params(body.Spec)
	}
	img.Username, img.PreinstalledDocker, img.Notes = body.Username, body.PreinstalledDocker, body.Notes
	if err := s.db.WithContext(r.Context()).Save(&img).Error; err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, img)
}

func (s *Server) apiDeleteImage(w http.ResponseWriter, r *http.Request) {
	if n := s.countPools(r, "image_id = ?", pathID(r)); n > 0 {
		writeJSONError(w, http.StatusConflict,
			"this image is still used by a pool; delete the pool first")
		return
	}
	if err := s.db.WithContext(r.Context()).Delete(&store.Image{}, pathID(r)).Error; err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// forgeBody is the JSON shape of a forge connection.
type forgeBody struct {
	Name     string         `json:"name"`
	Kind     string         `json:"kind"`
	Enabled  *bool          `json:"enabled,omitempty"`
	Settings map[string]any `json:"settings,omitempty"`
}

func (s *Server) apiCreateForge(w http.ResponseWriter, r *http.Request) {
	var body forgeBody
	if err := decode(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	impl, ok := forge.ByKind(forge.Kind(body.Kind))
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "unknown forge kind "+body.Kind)
		return
	}
	settings, creds := splitSettings(impl.Fields, body.Settings, nil)
	f := &store.Forge{
		Name: body.Name, Kind: body.Kind,
		Enabled: boolOr(body.Enabled, true), Settings: settings, Credentials: creds,
	}
	if f.Name == "" {
		writeJSONError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := s.db.WithContext(r.Context()).Create(f).Error; err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

func (s *Server) apiUpdateForge(w http.ResponseWriter, r *http.Request) {
	f, err := s.db.ForgeByID(r.Context(), pathID(r))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	var body forgeBody
	if err := decode(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	impl, ok := forge.ByKind(forge.Kind(f.Kind))
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "unknown forge kind "+f.Kind)
		return
	}
	if body.Name != "" {
		f.Name = body.Name
	}
	f.Enabled = boolOr(body.Enabled, f.Enabled)
	f.Settings, f.Credentials = splitSettings(impl.Fields, body.Settings, f.Credentials)
	if err := s.db.WithContext(r.Context()).Save(f).Error; err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// poolBody is the JSON shape of a pool.
type poolBody struct {
	Name           string   `json:"name"`
	Enabled        *bool    `json:"enabled,omitempty"`
	ForgeID        uint     `json:"forge_id"`
	CloudID        uint     `json:"cloud_id"`
	SizeID         uint     `json:"size_id"`
	ImageID        *uint    `json:"image_id,omitempty"`
	Labels         []string `json:"labels"`
	MinIdle        int      `json:"min_idle,omitempty"`
	MaxInstances   int      `json:"max_instances"`
	JobTimeoutSec  int      `json:"job_timeout_sec"`
	MaxLifetimeSec int      `json:"max_lifetime_sec"`
	ContainerImage string   `json:"container_image,omitempty"`
	PublicIPv4     *bool    `json:"public_ipv4,omitempty"`
	AllowSSHFrom   []string `json:"allow_ssh_from,omitempty"`
}

func (b poolBody) apply(p *store.Pool) {
	if b.Name != "" {
		p.Name = b.Name
	}
	p.Enabled = boolOr(b.Enabled, p.Enabled)
	p.ForgeID, p.CloudID, p.SizeID = b.ForgeID, b.CloudID, b.SizeID
	p.ImageID = b.ImageID
	p.Labels = store.StringList(b.Labels)
	p.MinIdle, p.MaxInstances = b.MinIdle, b.MaxInstances
	p.JobTimeoutSec, p.MaxLifetimeSec = b.JobTimeoutSec, b.MaxLifetimeSec
	p.ContainerImage = b.ContainerImage
	p.PublicIPv4 = boolOr(b.PublicIPv4, p.PublicIPv4)
	p.AllowSSHFrom = store.StringList(b.AllowSSHFrom)
}

func (s *Server) apiCreatePool(w http.ResponseWriter, r *http.Request) {
	var body poolBody
	if err := decode(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	p := &store.Pool{Enabled: true, PublicIPv4: true}
	body.apply(p)
	// The same validation the forms apply, so a pool created as code cannot be
	// one the UI would have refused.
	if err := validatePool(p); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.db.WithContext(r.Context()).Create(p).Error; err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) apiUpdatePool(w http.ResponseWriter, r *http.Request) {
	p, err := s.db.PoolByID(r.Context(), pathID(r))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	var body poolBody
	if err := decode(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	body.apply(p)
	if err := validatePool(p); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Preloaded associations must not be written back as rows of their own.
	p.Forge, p.Cloud, p.Size, p.Image = nil, nil, nil, nil
	if err := s.db.WithContext(r.Context()).Save(p).Error; err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// boolOr resolves an optional boolean, so omitting a field means "leave it"
// rather than "set it false".
func boolOr(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}
