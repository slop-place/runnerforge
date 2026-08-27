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
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/slop-place/runnerforge/internal/auth"
	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/config"
	"github.com/slop-place/runnerforge/internal/controller"
	"github.com/slop-place/runnerforge/internal/forge"
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
	auth *auth.Authenticator
}

// New builds the UI server. A nil authenticator leaves the UI ungated.
func New(
	db *store.DB, ctrl *controller.Controller, cfg *config.Config,
	log *slog.Logger, authn *auth.Authenticator,
) *Server {
	s := &Server{db: db, ctrl: ctrl, cfg: cfg, log: log, auth: authn}
	s.tpl = mustParse()
	return s
}

const (
	// checkTimeout bounds a credential check triggered from the UI.
	checkTimeout = 30 * time.Second
	// destroyTimeout bounds a single machine teardown triggered from the UI.
	destroyTimeout = 60 * time.Second
	// reapTimeout bounds a manual reap triggered from the UI.
	reapTimeout = 2 * time.Minute
	// centsThreshold is where a cost stops needing sub-cent precision. A job
	// that lasted a minute genuinely costs less than a cent, and rounding that
	// to $0.00 would hide the only number on the page worth watching.
	centsThreshold = 0.01
	// costWindow is how far back the spend figures look.
	costWindow = 24 * time.Hour
	// listLimit is how many rows the machine and event tables show.
	listLimit = 50
	// secondsPerMinute and minutesPerHour format ages in the tables.
	secondsPerMinute = 60
	minutesPerHour   = 60
)

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
			return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%minutesPerHour)
		}
	},
	// Timestamps are rendered in the server's local zone on purpose: this is an
	// operator console, and an operator reads it alongside their own clock.
	"ts": func(t time.Time) string { return t.Local().Format("15:04:05") }, //nolint:gosmopolitan // operator-facing local time
	// usd renders a cost. Sub-cent figures are the normal case for a job that
	// lasted a minute, so they are not rounded away to $0.00.
	"usd": func(v float64) string {
		if v == 0 {
			return "—"
		}
		if v < centsThreshold {
			return fmt.Sprintf("$%.4f", v)
		}
		return fmt.Sprintf("$%.2f", v)
	},
	// dur renders a billed duration compactly.
	"dur": func(d time.Duration) string {
		if d <= 0 {
			return "—"
		}
		if d < time.Minute {
			return fmt.Sprintf("%ds", int(d.Seconds()))
		}
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%secondsPerMinute)
	},
	"labels": func(l store.StringList) string { return strings.Join(l, ",") },
	// specsummary renders a driver spec as readable key=value pairs. The raw
	// JSON was accurate and unreadable; this is what an operator scans a table
	// for.
	"specsummary": func(p store.Params) string {
		if len(p) == 0 {
			return "—"
		}
		keys := make([]string, 0, len(p))
		for k := range p {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+paramString(p, k))
		}
		return strings.Join(parts, "  ")
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
			"templates/layout.html", "templates/partials.html",
			"templates/fields.html", "templates/"+p+".html"))
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

	s.auth.Routes(mux)
	return s.auth.Middleware(mux)
}

// view is the data every page template receives.
type view struct {
	Title     string
	Nav       string
	Flash     string
	FlashKind string

	// User is who is signed in, and Authenticated says whether the UI is gated
	// at all. An ungated UI says so on every page rather than looking secure.
	User          string
	Authenticated bool

	Stats     stats
	Clouds    []store.Cloud
	Forges    []store.Forge
	Pools     []poolRow
	Instances []store.Instance
	Events    []store.Event

	Cloud    *store.Cloud
	Forge    *store.Forge
	Pool     *store.Pool
	Instance *store.Instance

	// Fields are the driver-declared inputs for the record being edited.
	Fields []renderField
	// SizeFields and ImageFields drive the catalogue editors.
	SizeFields  []renderField
	ImageFields []renderField
	// CatalogErr explains why the pickers are empty, when they are.
	CatalogErr string

	CloudDrivers []cloud.Driver
	ForgeKinds   []forge.Implementation

	Sizes  []store.Size
	Images []store.Image

	WebhookURL       string
	HasWebhookSecret bool
}

type stats struct {
	Pools, Live, Busy, Failed int
	// Spend is what the last day has cost, machines still running included.
	Spend store.Spend
}

type poolRow struct {
	Pool  store.Pool
	Live  int
	Spend store.Spend
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
//
// The destination is rebuilt rather than concatenated: `to` is always a path
// this package chose, and the message is carried as a properly escaped query
// value, so neither can steer the redirect somewhere else.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, to string, err error) {
	s.log.Warn("request failed", "path", r.URL.Path, "err", err)
	s.redirect(w, r, to, "err", err.Error())
}

// redirect sends the client to a path chosen by this package.
//
// Every redirect in this file goes through here so there is exactly one place
// where a destination is built, and it can never be anything but a plain
// absolute path on this server.
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, path, key, value string) {
	//nolint:gosec // G710: internalRedirect rejects anything that is not a
	// same-origin absolute path, and the message is an escaped query value.
	http.Redirect(w, r, internalRedirect(path, key, value), http.StatusSeeOther)
}

// internalRedirect builds a same-origin redirect target, rejecting anything
// that is not a plain absolute path on this server.
func internalRedirect(path, key, value string) string {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		path = "/"
	}
	u := &url.URL{Path: path}
	if key != "" {
		u.RawQuery = url.Values{key: {value}}.Encode()
	}
	return u.String()
}

func (s *Server) base(r *http.Request, title, nav string) view {
	v := view{Title: title, Nav: nav, Authenticated: s.auth.Enabled()}
	if u, ok := auth.UserFrom(r.Context()); ok {
		v.User = u.Display()
	}
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
	// A day is the window an operator can still do something about.
	if sp, err := s.db.SpendSince(ctx, time.Now().Add(-costWindow)); err == nil {
		v.Stats.Spend = sp
	}
	v.Stats.Pools = len(pools)
	for _, in := range live {
		v.Stats.Live++
		switch in.State {
		case store.StateBusy:
			v.Stats.Busy++
		case store.StateFailed:
			v.Stats.Failed++
		case store.StatePending, store.StateProvisioning, store.StateBooting,
			store.StateIdle, store.StateDraining, store.StateDeleted:
			// Counted in Live above; no separate tile of their own.
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
	v.CloudDrivers = cloud.Drivers()
	// The connection form is driver-specific, so it appears once a driver is
	// picked rather than as a JSON textarea that works for none of them.
	if d := r.URL.Query().Get("driver"); d != "" {
		if drv, ok := cloud.DriverByName(d); ok {
			v.Fields = buildFields(drv.Schema.Connection, nil, nil)
			v.Cloud = &store.Cloud{Driver: d}
		}
	}
	s.render(w, "clouds", v)
}

func (s *Server) createCloud(w http.ResponseWriter, r *http.Request) {
	driver := r.FormValue("driver")
	drv, ok := cloud.DriverByName(driver)
	if !ok {
		s.fail(w, r, "/clouds", fmt.Errorf("unknown driver %q", driver))
		return
	}
	back := "/clouds?driver=" + url.QueryEscape(driver)

	settings, creds, err := collectFields(r, drv.Schema.Connection, nil)
	if err != nil {
		s.fail(w, r, back, err)
		return
	}
	c := &store.Cloud{
		Name: strings.TrimSpace(r.FormValue("name")), Driver: driver,
		Enabled: true, Settings: settings, Credentials: creds,
	}
	if c.Name == "" {
		s.fail(w, r, back, errNameRequired)
		return
	}
	if err := s.db.WithContext(r.Context()).Create(c).Error; err != nil {
		s.fail(w, r, back, err)
		return
	}
	s.redirect(w, r, fmt.Sprintf("/clouds/%d", c.ID), "", "")
}

func (s *Server) editCloud(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.CloudByID(r.Context(), pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	drv, ok := cloud.DriverByName(c.Driver)
	if !ok {
		s.fail(w, r, "/clouds", fmt.Errorf("unknown driver %q", c.Driver))
		return
	}
	v := s.base(r, c.Name, "clouds")
	v.Cloud = c
	v.Fields = buildFields(drv.Schema.Connection, c.Settings, c.Credentials)
	v.SizeFields = buildFields(drv.Schema.Size, nil, nil)
	v.ImageFields = buildFields(drv.Schema.Image, nil, nil)

	// A driver that can list what the account offers turns the flavor and
	// image inputs into pickers of real values.
	if flavors, images, err := s.catalog(r.Context(), c); err != nil {
		v.CatalogErr = err.Error()
	} else {
		v.SizeFields = withCatalog(v.SizeFields, "flavor", flavors)
		v.ImageFields = withCatalog(v.ImageFields, "id", images)
	}
	s.render(w, "cloud_edit", v)
}

func (s *Server) updateCloud(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := s.db.CloudByID(ctx, pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	self := fmt.Sprintf("/clouds/%d", c.ID)
	drv, ok := cloud.DriverByName(c.Driver)
	if !ok {
		s.fail(w, r, "/clouds", fmt.Errorf("unknown driver %q", c.Driver))
		return
	}
	// Existing credentials are passed in so a field left blank keeps its
	// stored value rather than erasing a secret nobody could see.
	settings, creds, err := collectFields(r, drv.Schema.Connection, c.Credentials)
	if err != nil {
		s.fail(w, r, self, err)
		return
	}
	c.Name = strings.TrimSpace(r.FormValue("name"))
	c.Enabled = r.FormValue("enabled") != ""
	c.Settings = settings
	c.Credentials = creds
	if err := s.db.WithContext(ctx).Save(c).Error; err != nil {
		s.fail(w, r, self, err)
		return
	}
	s.redirect(w, r, self, "ok", "saved")
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
	s.redirect(w, r, "/clouds", "ok", "deleted")
}

func (s *Server) checkCloud(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
	defer cancel()
	c, err := s.db.CloudByID(ctx, pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.ctrl.CheckCloud(ctx, c)
	s.redirect(w, r, "/clouds", "", "")
}

func (s *Server) createSize(w http.ResponseWriter, r *http.Request) {
	cloudID := pathID(r)
	self := fmt.Sprintf("/clouds/%d", cloudID)
	spec, err := s.specFromForm(r, cloudID, func(sc cloud.Schema) []cloud.Field { return sc.Size })
	if err != nil {
		s.fail(w, r, self, err)
		return
	}
	hourly, _ := strconv.ParseFloat(r.FormValue("hourly_usd"), 64)
	sz := &store.Size{
		CloudID: cloudID, Name: strings.TrimSpace(r.FormValue("name")), Spec: spec,
		VCPUs: atoi(r.FormValue("vcpus")), MemoryMB: atoi(r.FormValue("memory_mb")),
		HourlyUSD: hourly,
	}
	if sz.Name == "" {
		s.fail(w, r, self, errNameRequired)
		return
	}
	if err := s.db.WithContext(r.Context()).Create(sz).Error; err != nil {
		s.fail(w, r, self, err)
		return
	}
	s.redirect(w, r, self, "", "")
}

func (s *Server) createImage(w http.ResponseWriter, r *http.Request) {
	cloudID := pathID(r)
	self := fmt.Sprintf("/clouds/%d", cloudID)
	spec, err := s.specFromForm(r, cloudID, func(sc cloud.Schema) []cloud.Field { return sc.Image })
	if err != nil {
		s.fail(w, r, self, err)
		return
	}
	img := &store.Image{
		CloudID: cloudID, Name: strings.TrimSpace(r.FormValue("name")), Spec: spec,
		Username:           strings.TrimSpace(r.FormValue("username")),
		PreinstalledDocker: r.FormValue("preinstalled_docker") != "",
	}
	if img.Name == "" {
		s.fail(w, r, self, errNameRequired)
		return
	}
	if err := s.db.WithContext(r.Context()).Create(img).Error; err != nil {
		s.fail(w, r, self, err)
		return
	}
	s.redirect(w, r, self, "", "")
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
	v.ForgeKinds = forge.Implementations()
	if k := r.URL.Query().Get("kind"); k != "" {
		if impl, ok := forge.ByKind(forge.Kind(k)); ok {
			v.Fields = buildFields(impl.Fields, nil, nil)
			v.Forge = &store.Forge{Kind: k}
		}
	}
	s.render(w, "forges", v)
}

func (s *Server) createForge(w http.ResponseWriter, r *http.Request) {
	kind := r.FormValue("kind")
	impl, ok := forge.ByKind(forge.Kind(kind))
	if !ok {
		s.fail(w, r, "/forges", fmt.Errorf("unknown forge kind %q", kind))
		return
	}
	back := "/forges?kind=" + url.QueryEscape(kind)

	settings, creds, err := collectFields(r, impl.Fields, nil)
	if err != nil {
		s.fail(w, r, back, err)
		return
	}
	f := &store.Forge{
		Name: strings.TrimSpace(r.FormValue("name")), Kind: kind,
		Enabled: true, Settings: settings, Credentials: creds,
	}
	if f.Name == "" {
		s.fail(w, r, back, errNameRequired)
		return
	}
	if err := s.db.WithContext(r.Context()).Create(f).Error; err != nil {
		s.fail(w, r, back, err)
		return
	}
	s.redirect(w, r, fmt.Sprintf("/forges/%d", f.ID), "", "")
}

func (s *Server) editForge(w http.ResponseWriter, r *http.Request) {
	f, err := s.db.ForgeByID(r.Context(), pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	impl, ok := forge.ByKind(forge.Kind(f.Kind))
	if !ok {
		s.fail(w, r, "/forges", fmt.Errorf("unknown forge kind %q", f.Kind))
		return
	}
	v := s.base(r, f.Name, "forges")
	v.Forge = f
	v.Fields = buildFields(impl.Fields, f.Settings, f.Credentials)
	v.HasWebhookSecret = len(f.WebhookSecret) > 0
	if s.cfg.BaseURL != "" {
		v.WebhookURL = strings.TrimRight(s.cfg.BaseURL, "/") + "/webhooks/" + strconv.Itoa(int(f.ID))
	}
	s.render(w, "forge_edit", v)
}

func (s *Server) updateForge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f, err := s.db.ForgeByID(ctx, pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	self := fmt.Sprintf("/forges/%d", f.ID)
	impl, ok := forge.ByKind(forge.Kind(f.Kind))
	if !ok {
		s.fail(w, r, "/forges", fmt.Errorf("unknown forge kind %q", f.Kind))
		return
	}
	settings, creds, err := collectFields(r, impl.Fields, f.Credentials)
	if err != nil {
		s.fail(w, r, self, err)
		return
	}
	f.Name = strings.TrimSpace(r.FormValue("name"))
	f.Enabled = r.FormValue("enabled") != ""
	f.Settings = settings
	f.Credentials = creds
	if ws := strings.TrimSpace(r.FormValue("webhook_secret")); ws != "" {
		f.WebhookSecret = store.Secret{"secret": ws}
	}
	if err := s.db.WithContext(ctx).Save(f).Error; err != nil {
		s.fail(w, r, self, err)
		return
	}
	s.redirect(w, r, self, "ok", "saved")
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
	s.redirect(w, r, "/forges", "ok", "deleted")
}

func (s *Server) checkForge(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
	defer cancel()
	f, err := s.db.ForgeByID(ctx, pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.ctrl.CheckForge(ctx, f)
	s.redirect(w, r, "/forges", "", "")
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
	s.redirect(w, r, "/pools", "ok", "created")
}

func (s *Server) editPool(w http.ResponseWriter, r *http.Request) {
	p, err := s.db.PoolByID(r.Context(), pathID(r))
	if err != nil {
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
	if err != nil {
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
	s.redirect(w, r, self, "ok", "saved")
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
	s.redirect(w, r, "/pools", "ok", "deleted")
}

// validatePool catches the configuration mistakes that would otherwise surface
// only when a job is already waiting for a machine that will never arrive.
func validatePool(p *store.Pool) error {
	switch {
	case p.Name == "":
		return errors.New("name is required")
	case p.ForgeID == 0:
		return errors.New("a forge is required")
	case p.CloudID == 0:
		return errors.New("a cloud is required")
	case p.SizeID == 0:
		return errors.New("a size is required; add one to the cloud first")
	case len(p.Labels) == 0:
		return errors.New("at least one label is required, or no job can select this pool")
	case p.MaxInstances < 1:
		return errors.New("max machines must be at least 1")
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
	ctx, cancel := context.WithTimeout(r.Context(), destroyTimeout)
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
	ctx, cancel := context.WithTimeout(r.Context(), reapTimeout)
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
	since := time.Now().Add(-costWindow)
	for i := range pools {
		live, _ := s.db.LiveInstances(ctx, pools[i].ID)
		spend, _ := s.db.PoolSpendSince(ctx, pools[i].ID, since)
		v.Pools = append(v.Pools, poolRow{Pool: pools[i], Live: len(live), Spend: spend})
	}
	s.renderPartial(w, "pools-table", v)
}

func (s *Server) partialInstances(w http.ResponseWriter, r *http.Request) {
	insts, err := s.db.RecentInstances(r.Context(), listLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderPartial(w, "instances-table", view{Instances: insts})
}

func (s *Server) partialEvents(w http.ResponseWriter, r *http.Request) {
	evs, err := s.db.Events(r.Context(), listLimit)
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
	if err != nil {
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
	for p := range strings.SplitSeq(s, ",") {
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
