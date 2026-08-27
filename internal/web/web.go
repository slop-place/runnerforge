// Package web serves runnerforge's management UI.
//
// Clouds, forges and pools are configured here rather than in a file, so the
// handlers below are the actual source of truth for what the controller runs.
// The UI is server-rendered with htmx: the few things that need to feel live —
// machine state, events — poll small HTML fragments rather than shipping a
// client-side application.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/config"
	"github.com/slop-place/runnerforge/internal/controller"
	"github.com/slop-place/runnerforge/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Server renders the management UI.
type Server struct {
	db   *store.DB
	ctrl *controller.Controller
	cfg  *config.Config
	log  *slog.Logger
	tpl  map[string]*template.Template
}

// New builds the UI server.
func New(db *store.DB, ctrl *controller.Controller, cfg *config.Config, log *slog.Logger) *Server {
	s := &Server{db: db, ctrl: ctrl, cfg: cfg, log: log}
	s.tpl = mustParse()
	return s
}

// funcs are the helpers templates rely on for formatting.
var funcs = template.FuncMap{
	"age": func(t time.Time) string {
		d := time.Since(t)
		switch {
		case d < time.Minute:
			return fmt.Sprintf("%ds", int(d.Seconds()))
		case d < time.Hour:
			return fmt.Sprintf("%dm", int(d.Minutes()))
		default:
			return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
		}
	},
	"ts":     func(t time.Time) string { return t.Local().Format("15:04:05") },
	"labels": func(l store.StringList) string { return strings.Join(l, ",") },
	"specjson": func(p store.Params) string {
		b, err := json.Marshal(p)
		if err != nil {
			return ""
		}
		return string(b)
	},
	// scope renders a forge's target compactly for the list view.
	"scope": func(p store.Params) string {
		switch {
		case p.String("repo") != "":
			return p.String("owner") + "/" + p.String("repo")
		case p.String("owner") != "":
			return p.String("owner")
		case p.String("project_id") != "":
			return "project " + p.String("project_id")
		case p.String("group_id") != "":
			return "group " + p.String("group_id")
		default:
			return p.String("scope")
		}
	},
}

// pages maps a page template to the layout it renders inside.
var pages = []string{
	"dashboard", "clouds", "cloud_edit", "forges", "forge_edit",
	"pools", "pool_edit", "instances", "instance", "events",
}

func mustParse() map[string]*template.Template {
	out := map[string]*template.Template{}
	for _, p := range pages {
		t := template.Must(template.New(p).Funcs(funcs).ParseFS(templateFS,
			"templates/layout.html", "templates/partials.html", "templates/"+p+".html"))
		out[p] = t
	}
	// Partials render on their own, without a layout.
	out["_partials"] = template.Must(template.New("partials").Funcs(funcs).
		ParseFS(templateFS, "templates/partials.html", "templates/pools.html"))
	return out
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.FileServerFS(staticFS))

	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("GET /clouds", s.clouds)
	mux.HandleFunc("POST /clouds", s.createCloud)
	mux.HandleFunc("GET /clouds/{id}", s.editCloud)
	mux.HandleFunc("POST /clouds/{id}", s.updateCloud)
	mux.HandleFunc("GET /clouds/{id}/delete", s.deleteCloud)
	mux.HandleFunc("POST /clouds/{id}/check", s.checkCloud)
	mux.HandleFunc("POST /clouds/{id}/sizes", s.createSize)
	mux.HandleFunc("POST /clouds/{id}/images", s.createImage)
	mux.HandleFunc("POST /sizes/{id}/delete", s.deleteSize)
	mux.HandleFunc("POST /images/{id}/delete", s.deleteImage)

	mux.HandleFunc("GET /forges", s.forges)
	mux.HandleFunc("POST /forges", s.createForge)
	mux.HandleFunc("GET /forges/{id}", s.editForge)
	mux.HandleFunc("POST /forges/{id}", s.updateForge)
	mux.HandleFunc("GET /forges/{id}/delete", s.deleteForge)
	mux.HandleFunc("POST /forges/{id}/check", s.checkForge)

	mux.HandleFunc("GET /pools", s.pools)
	mux.HandleFunc("POST /pools", s.createPool)
	mux.HandleFunc("GET /pools/{id}", s.editPool)
	mux.HandleFunc("POST /pools/{id}", s.updatePool)
	mux.HandleFunc("GET /pools/{id}/delete", s.deletePool)

	mux.HandleFunc("GET /instances", s.instances)
	mux.HandleFunc("GET /instances/{id}", s.instance)
	mux.HandleFunc("POST /instances/{id}/destroy", s.destroyInstance)

	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("POST /reap", s.reap)

	mux.HandleFunc("GET /partials/pools", s.partialPools)
	mux.HandleFunc("GET /partials/instances", s.partialInstances)
	mux.HandleFunc("GET /partials/events", s.partialEvents)
	mux.HandleFunc("GET /partials/cloud-options", s.partialCloudOptions)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	return mux
}

// view is the data every page template receives.
type view struct {
	Title     string
	Nav       string
	Flash     string
	FlashKind string

	Stats     stats
	Clouds    []store.Cloud
	Forges    []store.Forge
	Pools     []poolRow
	Instances []store.Instance
	Events    []store.Event
	Drivers   []string

	Cloud        *store.Cloud
	Forge        *store.Forge
	Pool         *store.Pool
	Instance     *store.Instance
	SettingsJSON string
	HasCreds     bool
	CredKeys     string

	Sizes  []store.Size
	Images []store.Image

	WebhookURL       string
	HasWebhookSecret bool
}

type stats struct{ Pools, Live, Busy, Failed int }

type poolRow struct {
	Pool store.Pool
	Live int
}

func (s *Server) render(w http.ResponseWriter, page string, v view) {
	t, ok := s.tpl[page]
	if !ok {
		http.Error(w, "unknown page "+page, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", v); err != nil {
		s.log.Error("render", "page", page, "err", err)
	}
}

func (s *Server) renderPartial(w http.ResponseWriter, name string, v view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl["_partials"].ExecuteTemplate(w, name, v); err != nil {
		s.log.Error("render partial", "name", name, "err", err)
	}
}

// fail redirects back with an error message. Configuration mistakes are the
// common case here, so they are surfaced in the UI rather than logged and lost.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, to string, err error) {
	s.log.Warn("request failed", "path", r.URL.Path, "err", err)
	http.Redirect(w, r, to+"?err="+url_QueryEscape(err.Error()), http.StatusSeeOther)
}

func url_QueryEscape(s string) string {
	return strings.NewReplacer("&", "%26", "#", "%23", "?", "%3F", " ", "%20").Replace(s)
}

func (s *Server) base(r *http.Request, title, nav string) view {
	v := view{Title: title, Nav: nav}
	if e := r.URL.Query().Get("err"); e != "" {
		v.Flash, v.FlashKind = e, "bad"
	}
	if m := r.URL.Query().Get("ok"); m != "" {
		v.Flash, v.FlashKind = m, ""
	}
	return v
}

// ---- pages ----

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	v := s.base(r, "Dashboard", "dash")
	ctx := r.Context()

	pools, err := s.db.Pools(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	live, err := s.db.AllLiveInstances(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	v.Stats.Pools = len(pools)
	for _, in := range live {
		v.Stats.Live++
		switch in.State {
		case store.StateBusy:
			v.Stats.Busy++
		case store.StateFailed:
			v.Stats.Failed++
		}
	}
	s.render(w, "dashboard", v)
}

func (s *Server) clouds(w http.ResponseWriter, r *http.Request) {
	v := s.base(r, "Clouds", "clouds")
	var err error
	if v.Clouds, err = s.db.Clouds(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for name := range cloud.Drivers {
		v.Drivers = append(v.Drivers, name)
	}
	sort.Strings(v.Drivers)
	s.render(w, "clouds", v)
}

func (s *Server) createCloud(w http.ResponseWriter, r *http.Request) {
	settings, err := parseParams(r.FormValue("settings"))
	if err != nil {
		s.fail(w, r, "/clouds", fmt.Errorf("settings: %w", err))
		return
	}
	creds, err := parseSecret(r.FormValue("credentials"))
	if err != nil {
		s.fail(w, r, "/clouds", fmt.Errorf("credentials: %w", err))
		return
	}
	c := &store.Cloud{
		Name: strings.TrimSpace(r.FormValue("name")), Driver: r.FormValue("driver"),
		Enabled: true, Settings: settings, Credentials: creds,
	}
	if _, ok := cloud.Drivers[c.Driver]; !ok {
		s.fail(w, r, "/clouds", fmt.Errorf("unknown driver %q", c.Driver))
		return
	}
	if err := s.db.WithContext(r.Context()).Create(c).Error; err != nil {
		s.fail(w, r, "/clouds", err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/clouds/%d", c.ID), http.StatusSeeOther)
}

func (s *Server) editCloud(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.CloudByID(r.Context(), pathID(r))
	if err != nil || c == nil {
		http.NotFound(w, r)
		return
	}
	v := s.base(r, c.Name, "clouds")
	v.Cloud = c
	v.SettingsJSON = prettyJSON(c.Settings)
	v.HasCreds = len(c.Credentials) > 0
	v.CredKeys = strings.Join(sortedKeys(c.Credentials), ", ")
	s.render(w, "cloud_edit", v)
}

func (s *Server) updateCloud(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := s.db.CloudByID(ctx, pathID(r))
	if err != nil || c == nil {
		http.NotFound(w, r)
		return
	}
	self := fmt.Sprintf("/clouds/%d", c.ID)
	settings, err := parseParams(r.FormValue("settings"))
	if err != nil {
		s.fail(w, r, self, fmt.Errorf("settings: %w", err))
		return
	}
	c.Name = strings.TrimSpace(r.FormValue("name"))
	c.Enabled = r.FormValue("enabled") != ""
	c.Settings = settings
	// Blank credentials means "keep what is stored", so an operator editing a
	// region does not have to re-enter a secret they cannot see.
	if raw := strings.TrimSpace(r.FormValue("credentials")); raw != "" && raw != "{}" {
		creds, err := parseSecret(raw)
		if err != nil {
			s.fail(w, r, self, fmt.Errorf("credentials: %w", err))
			return
		}
		c.Credentials = creds
	}
	if err := s.db.WithContext(ctx).Save(c).Error; err != nil {
		s.fail(w, r, self, err)
		return
	}
	http.Redirect(w, r, self+"?ok=saved", http.StatusSeeOther)
}

func (s *Server) deleteCloud(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	// Refuse while pools still reference it: deleting the cloud would strand
	// any machine currently running on it, with nothing left to reap it.
	var n int64
	s.db.WithContext(r.Context()).Model(&store.Pool{}).Where("cloud_id = ?", id).Count(&n)
	if n > 0 {
		s.fail(w, r, "/clouds", fmt.Errorf("%d pool(s) still use this cloud; delete them first", n))
		return
	}
	if err := s.db.WithContext(r.Context()).Delete(&store.Cloud{}, id).Error; err != nil {
		s.fail(w, r, "/clouds", err)
		return
	}
	http.Redirect(w, r, "/clouds?ok=deleted", http.StatusSeeOther)
}

func (s *Server) checkCloud(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	c, err := s.db.CloudByID(ctx, pathID(r))
	if err != nil || c == nil {
		http.NotFound(w, r)
		return
	}
	s.ctrl.CheckCloud(ctx, c)
	http.Redirect(w, r, "/clouds", http.StatusSeeOther)
}

func (s *Server) createSize(w http.ResponseWriter, r *http.Request) {
	cloudID := pathID(r)
	self := fmt.Sprintf("/clouds/%d", cloudID)
	spec, err := parseParams(r.FormValue("spec"))
	if err != nil {
		s.fail(w, r, self, fmt.Errorf("spec: %w", err))
		return
	}
	hourly, _ := strconv.ParseFloat(r.FormValue("hourly_usd"), 64)
	sz := &store.Size{
		CloudID: cloudID, Name: strings.TrimSpace(r.FormValue("name")), Spec: spec,
		VCPUs: atoi(r.FormValue("vcpus")), MemoryMB: atoi(r.FormValue("memory_mb")),
		HourlyUSD: hourly,
	}
	if err := s.db.WithContext(r.Context()).Create(sz).Error; err != nil {
		s.fail(w, r, self, err)
		return
	}
	http.Redirect(w, r, self, http.StatusSeeOther)
}

func (s *Server) createImage(w http.ResponseWriter, r *http.Request) {
	cloudID := pathID(r)
	self := fmt.Sprintf("/clouds/%d", cloudID)
	spec, err := parseParams(r.FormValue("spec"))
	if err != nil {
		s.fail(w, r, self, fmt.Errorf("spec: %w", err))
		return
	}
	img := &store.Image{
		CloudID: cloudID, Name: strings.TrimSpace(r.FormValue("name")), Spec: spec,
		Username:           strings.TrimSpace(r.FormValue("username")),
		PreinstalledDocker: r.FormValue("preinstalled_docker") != "",
	}
	if err := s.db.WithContext(r.Context()).Create(img).Error; err != nil {
		s.fail(w, r, self, err)
		return
	}
	http.Redirect(w, r, self, http.StatusSeeOther)
}

func (s *Server) deleteSize(w http.ResponseWriter, r *http.Request) {
	s.db.WithContext(r.Context()).Delete(&store.Size{}, pathID(r))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) deleteImage(w http.ResponseWriter, r *http.Request) {
	s.db.WithContext(r.Context()).Delete(&store.Image{}, pathID(r))
	w.WriteHeader(http.StatusOK)
}

// ---- forges ----

func (s *Server) forges(w http.ResponseWriter, r *http.Request) {
	v := s.base(r, "Forges", "forges")
	var err error
	if v.Forges, err = s.db.Forges(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "forges", v)
}

func (s *Server) createForge(w http.ResponseWriter, r *http.Request) {
	settings, err := parseParams(r.FormValue("settings"))
	if err != nil {
		s.fail(w, r, "/forges", fmt.Errorf("settings: %w", err))
		return
	}
	creds, err := parseSecret(r.FormValue("credentials"))
	if err != nil {
		s.fail(w, r, "/forges", fmt.Errorf("credentials: %w", err))
		return
	}
	f := &store.Forge{
		Name: strings.TrimSpace(r.FormValue("name")), Kind: r.FormValue("kind"),
		Enabled: true, Settings: settings, Credentials: creds,
	}
	if err := s.db.WithContext(r.Context()).Create(f).Error; err != nil {
		s.fail(w, r, "/forges", err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/forges/%d", f.ID), http.StatusSeeOther)
}

func (s *Server) editForge(w http.ResponseWriter, r *http.Request) {
	f, err := s.db.ForgeByID(r.Context(), pathID(r))
	if err != nil || f == nil {
		http.NotFound(w, r)
		return
	}
	v := s.base(r, f.Name, "forges")
	v.Forge = f
	v.SettingsJSON = prettyJSON(f.Settings)
	v.HasCreds = len(f.Credentials) > 0
	v.CredKeys = strings.Join(sortedKeys(f.Credentials), ", ")
	v.HasWebhookSecret = len(f.WebhookSecret) > 0
	if s.cfg.BaseURL != "" {
		v.WebhookURL = strings.TrimRight(s.cfg.BaseURL, "/") + "/webhooks/" + strconv.Itoa(int(f.ID))
	}
	s.render(w, "forge_edit", v)
}

func (s *Server) updateForge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f, err := s.db.ForgeByID(ctx, pathID(r))
	if err != nil || f == nil {
		http.NotFound(w, r)
		return
	}
	self := fmt.Sprintf("/forges/%d", f.ID)
	settings, err := parseParams(r.FormValue("settings"))
	if err != nil {
		s.fail(w, r, self, fmt.Errorf("settings: %w", err))
		return
	}
	f.Name = strings.TrimSpace(r.FormValue("name"))
	f.Enabled = r.FormValue("enabled") != ""
	f.Settings = settings
	if raw := strings.TrimSpace(r.FormValue("credentials")); raw != "" && raw != "{}" {
		creds, err := parseSecret(raw)
		if err != nil {
			s.fail(w, r, self, fmt.Errorf("credentials: %w", err))
			return
		}
		f.Credentials = creds
	}
	if ws := strings.TrimSpace(r.FormValue("webhook_secret")); ws != "" {
		f.WebhookSecret = store.Secret{"secret": ws}
	}
	if err := s.db.WithContext(ctx).Save(f).Error; err != nil {
		s.fail(w, r, self, err)
		return
	}
	http.Redirect(w, r, self+"?ok=saved", http.StatusSeeOther)
}

func (s *Server) deleteForge(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	var n int64
	s.db.WithContext(r.Context()).Model(&store.Pool{}).Where("forge_id = ?", id).Count(&n)
	if n > 0 {
		s.fail(w, r, "/forges", fmt.Errorf("%d pool(s) still use this forge; delete them first", n))
		return
	}
	if err := s.db.WithContext(r.Context()).Delete(&store.Forge{}, id).Error; err != nil {
		s.fail(w, r, "/forges", err)
		return
	}
	http.Redirect(w, r, "/forges?ok=deleted", http.StatusSeeOther)
}

func (s *Server) checkForge(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	f, err := s.db.ForgeByID(ctx, pathID(r))
	if err != nil || f == nil {
		http.NotFound(w, r)
		return
	}
	s.ctrl.CheckForge(ctx, f)
	http.Redirect(w, r, "/forges", http.StatusSeeOther)
}

// ---- pools ----

func (s *Server) pools(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := s.base(r, "Pools", "pools")
	var err error
	if v.Forges, err = s.db.Forges(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if v.Clouds, err = s.db.Clouds(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(v.Clouds) > 0 {
		v.Sizes, v.Images = v.Clouds[0].Sizes, v.Clouds[0].Images
	}
	s.render(w, "pools", v)
}

func (s *Server) createPool(w http.ResponseWriter, r *http.Request) {
	p := &store.Pool{
		Name:           strings.TrimSpace(r.FormValue("name")),
		Enabled:        true,
		ForgeID:        uint(atoi(r.FormValue("forge_id"))),
		CloudID:        uint(atoi(r.FormValue("cloud_id"))),
		SizeID:         uint(atoi(r.FormValue("size_id"))),
		Labels:         splitLabels(r.FormValue("labels")),
		MinIdle:        atoi(r.FormValue("min_idle")),
		MaxInstances:   atoi(r.FormValue("max_instances")),
		JobTimeoutSec:  atoi(r.FormValue("job_timeout_sec")),
		MaxLifetimeSec: atoi(r.FormValue("max_lifetime_sec")),
		ContainerImage: strings.TrimSpace(r.FormValue("container_image")),
		PublicIPv4:     r.FormValue("public_ipv4") != "",
	}
	if id := atoi(r.FormValue("image_id")); id > 0 {
		u := uint(id)
		p.ImageID = &u
	}
	if err := validatePool(p); err != nil {
		s.fail(w, r, "/pools", err)
		return
	}
	if err := s.db.WithContext(r.Context()).Create(p).Error; err != nil {
		s.fail(w, r, "/pools", err)
		return
	}
	http.Redirect(w, r, "/pools?ok=created", http.StatusSeeOther)
}

func (s *Server) editPool(w http.ResponseWriter, r *http.Request) {
	p, err := s.db.PoolByID(r.Context(), pathID(r))
	if err != nil || p == nil {
		http.NotFound(w, r)
		return
	}
	v := s.base(r, p.Name, "pools")
	v.Pool = p
	s.render(w, "pool_edit", v)
}

func (s *Server) updatePool(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.db.PoolByID(ctx, pathID(r))
	if err != nil || p == nil {
		http.NotFound(w, r)
		return
	}
	self := fmt.Sprintf("/pools/%d", p.ID)
	p.Name = strings.TrimSpace(r.FormValue("name"))
	p.Enabled = r.FormValue("enabled") != ""
	p.Labels = splitLabels(r.FormValue("labels"))
	p.MinIdle = atoi(r.FormValue("min_idle"))
	p.MaxInstances = atoi(r.FormValue("max_instances"))
	p.JobTimeoutSec = atoi(r.FormValue("job_timeout_sec"))
	p.MaxLifetimeSec = atoi(r.FormValue("max_lifetime_sec"))
	p.ContainerImage = strings.TrimSpace(r.FormValue("container_image"))
	p.PublicIPv4 = r.FormValue("public_ipv4") != ""
	if err := validatePool(p); err != nil {
		s.fail(w, r, self, err)
		return
	}
	if err := s.db.WithContext(ctx).Save(p).Error; err != nil {
		s.fail(w, r, self, err)
		return
	}
	http.Redirect(w, r, self+"?ok=saved", http.StatusSeeOther)
}

func (s *Server) deletePool(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := pathID(r)
	// Machines outlive the pool row unless they are dealt with first, and the
	// reaper identifies them by pool name, so tear them down before deleting.
	live, err := s.db.LiveInstances(ctx, id)
	if err == nil && len(live) > 0 {
		s.fail(w, r, "/pools", fmt.Errorf(
			"%d machine(s) are still running in this pool; wait for them or destroy them first", len(live)))
		return
	}
	if err := s.db.WithContext(ctx).Delete(&store.Pool{}, id).Error; err != nil {
		s.fail(w, r, "/pools", err)
		return
	}
	http.Redirect(w, r, "/pools?ok=deleted", http.StatusSeeOther)
}

// validatePool catches the configuration mistakes that would otherwise surface
// only when a job is already waiting for a machine that will never arrive.
func validatePool(p *store.Pool) error {
	switch {
	case p.Name == "":
		return fmt.Errorf("name is required")
	case p.ForgeID == 0:
		return fmt.Errorf("a forge is required")
	case p.CloudID == 0:
		return fmt.Errorf("a cloud is required")
	case p.SizeID == 0:
		return fmt.Errorf("a size is required; add one to the cloud first")
	case len(p.Labels) == 0:
		return fmt.Errorf("at least one label is required, or no job can select this pool")
	case p.MaxInstances < 1:
		return fmt.Errorf("max machines must be at least 1")
	case p.MinIdle > p.MaxInstances:
		return fmt.Errorf("min idle (%d) cannot exceed max machines (%d)", p.MinIdle, p.MaxInstances)
	case p.MaxLifetimeSec <= p.JobTimeoutSec:
		return fmt.Errorf("max lifetime (%ds) must exceed the job timeout (%ds), "+
			"or the reaper will destroy machines mid-job", p.MaxLifetimeSec, p.JobTimeoutSec)
	}
	return nil
}

// ---- instances and events ----

func (s *Server) instances(w http.ResponseWriter, r *http.Request) {
	s.render(w, "instances", s.base(r, "Machines", "instances"))
}

func (s *Server) instance(w http.ResponseWriter, r *http.Request) {
	var in store.Instance
	err := s.db.WithContext(r.Context()).Preload("Pool").First(&in, pathID(r)).Error
	if err != nil {
		http.NotFound(w, r)
		return
	}
	v := s.base(r, in.Name, "instances")
	v.Instance = &in
	s.render(w, "instance", v)
}

func (s *Server) destroyInstance(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.ctrl.DestroyInstance(ctx, pathID(r)); err != nil {
		s.log.Error("destroy instance", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	s.render(w, "events", s.base(r, "Events", "events"))
}

func (s *Server) reap(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := s.ctrl.Reap(ctx); err != nil {
		s.log.Error("manual reap", "err", err)
	}
	w.WriteHeader(http.StatusOK)
}

// ---- partials ----

func (s *Server) partialPools(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pools, err := s.db.Pools(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	v := view{}
	for i := range pools {
		live, _ := s.db.LiveInstances(ctx, pools[i].ID)
		v.Pools = append(v.Pools, poolRow{Pool: pools[i], Live: len(live)})
	}
	s.renderPartial(w, "pools-table", v)
}

func (s *Server) partialInstances(w http.ResponseWriter, r *http.Request) {
	insts, err := s.db.RecentInstances(r.Context(), 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderPartial(w, "instances-table", view{Instances: insts})
}

func (s *Server) partialEvents(w http.ResponseWriter, r *http.Request) {
	evs, err := s.db.Events(r.Context(), 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderPartial(w, "events-table", view{Events: evs})
}

// partialCloudOptions re-renders the size and image pickers when the selected
// cloud changes, since those catalogues are per cloud.
func (s *Server) partialCloudOptions(w http.ResponseWriter, r *http.Request) {
	id := uint(atoi(r.URL.Query().Get("cloud_id")))
	c, err := s.db.CloudByID(r.Context(), id)
	if err != nil || c == nil {
		s.renderPartial(w, "cloud-options", view{})
		return
	}
	s.renderPartial(w, "cloud-options", view{Sizes: c.Sizes, Images: c.Images})
}

// ---- helpers ----

func pathID(r *http.Request) uint {
	n, _ := strconv.Atoi(r.PathValue("id"))
	return uint(n)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func splitLabels(s string) store.StringList {
	var out store.StringList
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseParams(raw string) (store.Params, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return store.Params{}, nil
	}
	var p store.Params
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}
	return p, nil
}

func parseSecret(raw string) (store.Secret, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return store.Secret{}, nil
	}
	var s store.Secret
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("not valid JSON (expected an object of string values): %w", err)
	}
	return s, nil
}

func prettyJSON(p store.Params) string {
	if len(p) == 0 {
		return "{}"
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}

func sortedKeys(s store.Secret) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
