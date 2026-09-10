package k8s_test

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/slop-place/runnerforge/internal/k8s"
	"github.com/slop-place/runnerforge/internal/k8s/k8stest"
	"github.com/slop-place/runnerforge/internal/store"
)

// These tests drive the reconciler against a real API server: the CRDs from
// k8s/crds.yaml are installed, objects are created the way kubectl would, and
// the assertions read both runnerforge's database and the status subresources
// the API server holds. Skipped unless RF_TEST_KUBECONFIG points at a cluster.

// fixture is one test's cluster, namespace, database and reconciler.
type fixture struct {
	c   *k8stest.Cluster
	ns  string
	db  *store.DB
	rec *k8s.Reconciler
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	c := k8stest.Connect(t)
	c.InstallCRDs(t)
	ns := c.Namespace(t)

	if err := store.SetKey(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open("sqlite", t.TempDir()+"/k8s.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	rec, err := k8s.New(k8s.Config{Namespace: ns, Kubeconfig: c.Kubeconfig}, db, log)
	if err != nil {
		t.Fatalf("build reconciler: %v", err)
	}
	return &fixture{c: c, ns: ns, db: db, rec: rec}
}

// reconcile runs one pass and returns its error for the test to judge.
func (f *fixture) reconcile(t *testing.T) error {
	t.Helper()
	return f.rec.Reconcile(t.Context())
}

// mustReconcile runs one pass that is expected to be clean.
func (f *fixture) mustReconcile(t *testing.T) {
	t.Helper()
	if err := f.reconcile(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// qualified is the database name of an object in the test namespace.
func (f *fixture) qualified(name string) string {
	return f.ns + "/" + name
}

// The objects a working setup needs, in the shape the manifests use.

func dockerCloudSpec() map[string]any {
	return map[string]any{
		"driver":   "docker",
		"settings": map[string]any{},
		"sizes": []any{
			map[string]any{
				"name":      "small",
				"spec":      map[string]any{"cpus": "2", "memory_mb": "2048"},
				"vcpus":     int64(2),
				"memoryMB":  int64(2048),
				"hourlyUSD": 0.05,
			},
		},
		"images": []any{
			map[string]any{
				"name":               "runner",
				"spec":               map[string]any{"image": "code.forgejo.org/forgejo/runner:12"},
				"preinstalledDocker": true,
			},
		},
	}
}

func forgejoForgeSpec(secret string) map[string]any {
	return map[string]any{
		"kind": "forgejo",
		"settings": map[string]any{
			"url": "http://forge.internal", "scope": "repo", "owner": "o", "repo": "r",
		},
		"secretRef": map[string]any{"name": secret},
	}
}

func poolSpec() map[string]any {
	return map[string]any{
		"forgeRef":           "fj",
		"cloudRef":           "dock",
		"size":               "small",
		"image":              "runner",
		"labels":             []any{"linux", "x64"},
		"maxInstances":       int64(3),
		"jobTimeoutSeconds":  int64(120),
		"maxLifetimeSeconds": int64(300),
	}
}

func (f *fixture) createTrio(t *testing.T) {
	t.Helper()
	f.c.Secret(t, f.ns, "forge-token", map[string]string{"token": "t0k-secret"})
	f.c.Create(t, f.ns, "clouds", "dock", dockerCloudSpec())
	f.c.Create(t, f.ns, "forges", "fj", forgejoForgeSpec("forge-token"))
	f.c.Create(t, f.ns, "pools", "ci", poolSpec())
}

func (f *fixture) cloud(t *testing.T, name string) *store.Cloud {
	t.Helper()
	var c store.Cloud
	if err := f.db.Where("name = ?", name).First(&c).Error; err != nil {
		t.Fatalf("cloud %q: %v", name, err)
	}
	return &c
}

func (f *fixture) forge(t *testing.T, name string) *store.Forge {
	t.Helper()
	var fg store.Forge
	if err := f.db.Where("name = ?", name).First(&fg).Error; err != nil {
		t.Fatalf("forge %q: %v", name, err)
	}
	return &fg
}

func (f *fixture) pool(t *testing.T, name string) *store.Pool {
	t.Helper()
	var p store.Pool
	if err := f.db.Where("name = ?", name).First(&p).Error; err != nil {
		t.Fatalf("pool %q: %v", name, err)
	}
	return &p
}

func (f *fixture) count(t *testing.T, model any, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.db.Model(model).Where(query, args...).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func TestKubernetesReconcileAppliesObjects(t *testing.T) {
	f := newFixture(t)
	f.createTrio(t)
	f.mustReconcile(t)

	// Names are namespaced in the database, since it has no namespaces of its
	// own and two namespaces may each hold a "ci".
	cl := f.cloud(t, f.qualified("dock"))
	if cl.Driver != "docker" || !cl.Enabled {
		t.Errorf("cloud = driver %q enabled %v", cl.Driver, cl.Enabled)
	}
	if !k8s.Managed(cl.Settings) {
		t.Error("a cloud from the cluster must carry the managed marker")
	}

	var sz store.Size
	if err := f.db.Where("cloud_id = ? AND name = ?", cl.ID, "small").First(&sz).Error; err != nil {
		t.Fatalf("size: %v", err)
	}
	if sz.VCPUs != 2 || sz.MemoryMB != 2048 || sz.HourlyUSD != 0.05 {
		t.Errorf("size = %+v", sz)
	}
	if sz.Spec.String("cpus") != "2" {
		t.Errorf("size spec = %v", sz.Spec)
	}
	var img store.Image
	if err := f.db.Where("cloud_id = ? AND name = ?", cl.ID, "runner").First(&img).Error; err != nil {
		t.Fatalf("image: %v", err)
	}
	if !img.PreinstalledDocker || img.Spec.String("image") == "" {
		t.Errorf("image = %+v", img)
	}

	// The credential is read from the Secret, never from the object.
	fg := f.forge(t, f.qualified("fj"))
	if fg.Credentials["token"] != "t0k-secret" {
		t.Errorf("forge token = %q, want the Secret's value", fg.Credentials["token"])
	}
	if fg.Settings.String("scope") != "repo" || !k8s.Managed(fg.Settings) {
		t.Errorf("forge settings = %v", fg.Settings)
	}

	p := f.pool(t, f.qualified("ci"))
	if p.ForgeID != fg.ID || p.CloudID != cl.ID || p.SizeID != sz.ID || p.ImageID == nil || *p.ImageID != img.ID {
		t.Errorf("pool references = forge %d cloud %d size %d image %v", p.ForgeID, p.CloudID, p.SizeID, p.ImageID)
	}
	if p.MaxInstances != 3 || p.JobTimeoutSec != 120 || p.MaxLifetimeSec != 300 {
		t.Errorf("pool limits = %d/%d/%d", p.MaxInstances, p.JobTimeoutSec, p.MaxLifetimeSec)
	}
	if strings.Join(p.Labels, ",") != "linux,x64" {
		t.Errorf("pool labels = %v", p.Labels)
	}
	// Fields the manifest left out take the CRD's documented defaults.
	if !p.Enabled || !p.PublicIPv4 || p.MinIdle != 0 {
		t.Errorf("pool defaults = enabled %v publicIPv4 %v minIdle %d", p.Enabled, p.PublicIPv4, p.MinIdle)
	}

	// And the API server was told what happened to each object.
	for _, tc := range []struct {
		resource, name string
		extra          string
	}{
		{"clouds", "dock", "sizes"}, {"forges", "fj", "id"}, {"pools", "ci", "machines"},
	} {
		st := f.c.Status(t, f.ns, tc.resource, tc.name)
		if st.Phase() != "Ready" {
			t.Errorf("%s %s: phase %q message %q, want Ready", tc.resource, tc.name, st.Phase(), st.Message())
		}
		if !st.Current() {
			t.Errorf("%s %s: status observed generation %d, object at %d",
				tc.resource, tc.name, st.Int("observedGeneration"), st.Generation)
		}
		if st.Int("id") <= 0 {
			t.Errorf("%s %s: status carries no database id", tc.resource, tc.name)
		}
		if st.Int(tc.extra) < 0 {
			t.Errorf("%s %s: status has no %s field", tc.resource, tc.name, tc.extra)
		}
	}
	if got := f.c.Status(t, f.ns, "clouds", "dock"); got.Int("sizes") != 1 || got.Int("images") != 1 {
		t.Errorf("cloud status counts = sizes %d images %d", got.Int("sizes"), got.Int("images"))
	}
	if got := f.c.Status(t, f.ns, "pools", "ci").Int("machines"); got != 0 {
		t.Errorf("pool status machines = %d, want 0", got)
	}

	// A second pass is a no-op: same rows, not duplicates.
	f.mustReconcile(t)
	if n := f.count(t, &store.Cloud{}, "1 = 1"); n != 1 {
		t.Errorf("%d clouds after two passes, want 1", n)
	}
	if n := f.count(t, &store.Pool{}, "1 = 1"); n != 1 {
		t.Errorf("%d pools after two passes, want 1", n)
	}
}

func TestKubernetesEditsFollowTheObject(t *testing.T) {
	f := newFixture(t)
	f.createTrio(t)
	f.mustReconcile(t)
	cl := f.cloud(t, f.qualified("dock"))

	// Raise the pool's ceiling and disable the forge, as an operator editing
	// manifests would.
	spec := poolSpec()
	spec["maxInstances"] = int64(7)
	f.c.Update(t, f.ns, "pools", "ci", spec)
	fspec := forgejoForgeSpec("forge-token")
	fspec["enabled"] = false
	f.c.Update(t, f.ns, "forges", "fj", fspec)

	// Add a size and drop the one the pool uses; the used one has to survive.
	cspec := dockerCloudSpec()
	cspec["sizes"] = []any{map[string]any{
		"name": "big", "spec": map[string]any{"cpus": "8"}, "vcpus": int64(8),
	}}
	cspec["images"] = []any{}
	f.c.Update(t, f.ns, "clouds", "dock", cspec)

	f.mustReconcile(t)

	if p := f.pool(t, f.qualified("ci")); p.MaxInstances != 7 {
		t.Errorf("pool maxInstances = %d after edit, want 7", p.MaxInstances)
	}
	if fg := f.forge(t, f.qualified("fj")); fg.Enabled {
		t.Error("forge still enabled after the object disabled it")
	}
	for _, name := range []string{"small", "big"} {
		if n := f.count(t, &store.Size{}, "cloud_id = ? AND name = ?", cl.ID, name); n != 1 {
			t.Errorf("size %q: %d rows, want 1 (small is in use, big is new)", name, n)
		}
	}
	if n := f.count(t, &store.Image{}, "cloud_id = ?", cl.ID); n != 1 {
		t.Errorf("%d images, want the in-use one kept", n)
	}
	st := f.c.Status(t, f.ns, "clouds", "dock")
	if st.Int("sizes") != 2 || !st.Current() {
		t.Errorf("cloud status = sizes %d current %v", st.Int("sizes"), st.Current())
	}

	// Now the pool moves off the old size, and the next pass may prune it.
	spec["size"] = "big"
	spec["image"] = ""
	f.c.Update(t, f.ns, "pools", "ci", spec)
	f.mustReconcile(t)
	f.mustReconcile(t)
	if n := f.count(t, &store.Size{}, "cloud_id = ? AND name = ?", cl.ID, "small"); n != 0 {
		t.Error("the unused size was not pruned once nothing referenced it")
	}
	if n := f.count(t, &store.Image{}, "cloud_id = ?", cl.ID); n != 0 {
		t.Error("the unused image was not pruned once nothing referenced it")
	}
	if p := f.pool(t, f.qualified("ci")); p.ImageID != nil {
		t.Error("pool still carries an image after the manifest dropped it")
	}
}

func TestKubernetesPoolReportsWhatItCannotResolve(t *testing.T) {
	f := newFixture(t)
	f.c.Create(t, f.ns, "pools", "ci", poolSpec())

	if err := f.reconcile(t); err == nil {
		t.Error("a pool whose forge does not exist should fail the pass")
	}
	st := f.c.Status(t, f.ns, "pools", "ci")
	if st.Phase() != "Error" || !strings.Contains(st.Message(), "not found") {
		t.Errorf("pool status = %q %q, want an Error naming what is missing", st.Phase(), st.Message())
	}
	if n := f.count(t, &store.Pool{}, "1 = 1"); n != 0 {
		t.Error("an unresolvable pool must not be written to the database")
	}

	// The references appear; the same object now resolves without an edit.
	f.c.Secret(t, f.ns, "forge-token", map[string]string{"token": "x"})
	f.c.Create(t, f.ns, "clouds", "dock", dockerCloudSpec())
	f.c.Create(t, f.ns, "forges", "fj", forgejoForgeSpec("forge-token"))
	f.mustReconcile(t)
	if st := f.c.Status(t, f.ns, "pools", "ci"); st.Phase() != "Ready" {
		t.Errorf("pool status = %q %q after its references appeared", st.Phase(), st.Message())
	}

	// The lifetime rule the UI enforces holds for manifests too.
	bad := poolSpec()
	bad["maxLifetimeSeconds"] = int64(120)
	f.c.Create(t, f.ns, "pools", "short", bad)
	if err := f.reconcile(t); err == nil {
		t.Error("a pool with maxLifetime <= jobTimeout should fail the pass")
	}
	if st := f.c.Status(t, f.ns, "pools", "short"); st.Phase() != "Error" || !strings.Contains(st.Message(), "must exceed") {
		t.Errorf("short pool status = %q %q", st.Phase(), st.Message())
	}
	if n := f.count(t, &store.Pool{}, "name = ?", f.qualified("short")); n != 0 {
		t.Error("a pool the reaper would kill mid-job was written anyway")
	}
}

func TestKubernetesMissingSecretFailsOnlyThatObject(t *testing.T) {
	f := newFixture(t)
	f.c.Create(t, f.ns, "clouds", "dock", dockerCloudSpec())
	f.c.Create(t, f.ns, "forges", "fj", forgejoForgeSpec("no-such-secret"))

	if err := f.reconcile(t); err == nil {
		t.Error("a forge whose Secret is missing should fail the pass")
	}
	if st := f.c.Status(t, f.ns, "forges", "fj"); st.Phase() != "Error" || !strings.Contains(st.Message(), "no-such-secret") {
		t.Errorf("forge status = %q %q, want an Error naming the Secret", st.Phase(), st.Message())
	}
	if n := f.count(t, &store.Forge{}, "1 = 1"); n != 0 {
		t.Error("a forge without its credential must not be written")
	}
	// The cloud beside it is unaffected.
	if st := f.c.Status(t, f.ns, "clouds", "dock"); st.Phase() != "Ready" {
		t.Errorf("cloud status = %q, want Ready despite the forge's failure", st.Phase())
	}
}

func TestKubernetesDeletionPrunesOnlyManagedRecords(t *testing.T) {
	f := newFixture(t)
	f.createTrio(t)

	// A cloud and a forge made in the UI: no managed marker.
	ui := &store.Cloud{Name: "ui-cloud", Driver: "docker", Enabled: true, Settings: store.Params{}}
	if err := f.db.Create(ui).Error; err != nil {
		t.Fatal(err)
	}
	uiForge := &store.Forge{Name: "ui-forge", Kind: "forgejo", Enabled: true,
		Settings: store.Params{"url": "http://x"}, Credentials: store.Secret{"token": "t"}}
	if err := f.db.Create(uiForge).Error; err != nil {
		t.Fatal(err)
	}
	f.mustReconcile(t)

	// Delete the pool from the cluster: its record goes, the others stay
	// because they still exist as objects.
	f.c.Delete(t, f.ns, "pools", "ci")
	f.mustReconcile(t)
	if n := f.count(t, &store.Pool{}, "1 = 1"); n != 0 {
		t.Error("pool record survived the object's deletion")
	}
	if n := f.count(t, &store.Cloud{}, "1 = 1"); n != 2 {
		t.Errorf("%d clouds, want the managed one and the UI one", n)
	}

	// Delete the forge and the cloud: managed records go, UI records stay.
	f.c.Delete(t, f.ns, "forges", "fj")
	f.c.Delete(t, f.ns, "clouds", "dock")
	f.mustReconcile(t)
	if n := f.count(t, &store.Cloud{}, "name = ?", f.qualified("dock")); n != 0 {
		t.Error("managed cloud survived the object's deletion")
	}
	if n := f.count(t, &store.Forge{}, "name = ?", f.qualified("fj")); n != 0 {
		t.Error("managed forge survived the object's deletion")
	}
	if n := f.count(t, &store.Cloud{}, "name = ?", "ui-cloud"); n != 1 {
		t.Error("the UI-made cloud was removed by the reconciler")
	}
	if n := f.count(t, &store.Forge{}, "name = ?", "ui-forge"); n != 1 {
		t.Error("the UI-made forge was removed by the reconciler")
	}
	// The catalogue went with its cloud.
	if n := f.count(t, &store.Size{}, "1 = 1"); n != 0 {
		t.Errorf("%d sizes left after their cloud was removed", n)
	}
}

func TestKubernetesPoolWithMachinesOutlivesItsObject(t *testing.T) {
	f := newFixture(t)
	f.createTrio(t)
	f.mustReconcile(t)
	p := f.pool(t, f.qualified("ci"))

	// A machine is running for the pool.
	inst := &store.Instance{Name: "rf-ci-live", PoolID: p.ID, State: store.StateBusy}
	if err := f.db.Create(inst).Error; err != nil {
		t.Fatal(err)
	}
	f.mustReconcile(t)
	if got := f.c.Status(t, f.ns, "pools", "ci").Int("machines"); got != 1 {
		t.Errorf("pool status machines = %d, want 1", got)
	}

	// The object goes while the machine is still alive: the record must stay,
	// and so must the cloud it needs, or the machine could never be torn down.
	f.c.Delete(t, f.ns, "pools", "ci")
	f.c.Delete(t, f.ns, "clouds", "dock")
	f.mustReconcile(t)
	if n := f.count(t, &store.Pool{}, "id = ?", p.ID); n != 1 {
		t.Fatal("pool with a live machine was removed")
	}
	if n := f.count(t, &store.Cloud{}, "id = ?", p.CloudID); n != 1 {
		t.Fatal("cloud still referenced by a pool was removed")
	}

	// Once the machine is gone, the next pass finishes the job.
	if err := f.db.Model(inst).Update("state", store.StateDeleted).Error; err != nil {
		t.Fatal(err)
	}
	if live, err := f.db.LiveInstances(t.Context(), p.ID); err != nil || len(live) != 0 {
		t.Fatalf("live instances after marking deleted = %d (%v)", len(live), err)
	}
	f.mustReconcile(t)
	f.mustReconcile(t)
	if n := f.count(t, &store.Pool{}, "id = ?", p.ID); n != 0 {
		t.Error("pool was not removed once its machines were gone")
	}
	if n := f.count(t, &store.Cloud{}, "id = ?", p.CloudID); n != 0 {
		t.Error("cloud was not removed once nothing referenced it")
	}
}

func TestKubernetesReconcilerStaysInItsNamespace(t *testing.T) {
	f := newFixture(t)
	other := f.c.Namespace(t)
	f.c.Create(t, f.ns, "clouds", "dock", dockerCloudSpec())
	f.c.Create(t, other, "clouds", "dock", dockerCloudSpec())
	f.mustReconcile(t)

	if n := f.count(t, &store.Cloud{}, "1 = 1"); n != 1 {
		t.Errorf("%d clouds, want only the one in the watched namespace", n)
	}
	if st := f.c.Status(t, other, "clouds", "dock"); st.Phase() != "" {
		t.Errorf("an object outside the watched namespace got status %q", st.Phase())
	}
}
