package k8s

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/slop-place/runnerforge/internal/metrics"
	"github.com/slop-place/runnerforge/internal/store"
)

// specRoot is the top-level field every resource's configuration lives under.
const specRoot = "spec"

// Pool defaults, matching the CRD schema and the deployment's own forms.
const (
	defaultMaxInstances   = 5
	defaultJobTimeoutSec  = 3600
	defaultMaxLifetimeSec = 7200
)

// managedName is the record name a cluster object maps to.
//
// Namespaced so two namespaces can each hold a "ci" pool without colliding in
// a database that has no namespaces of its own.
func managedName(obj *unstructured.Unstructured) string {
	if ns := obj.GetNamespace(); ns != "" && ns != "default" {
		return ns + "/" + obj.GetName()
	}
	return obj.GetName()
}

// specString reads a string from a resource's spec.
func specString(obj *unstructured.Unstructured, path ...string) string {
	v, _, _ := unstructured.NestedString(obj.Object, append([]string{specRoot}, path...)...)
	return v
}

// specBool reads a boolean, defaulting when absent.
// specBool reads a boolean, defaulting to true when absent — which is what
// every boolean in these schemas defaults to.
func specBool(obj *unstructured.Unstructured, path ...string) bool {
	v, found, _ := unstructured.NestedBool(obj.Object, append([]string{specRoot}, path...)...)
	if !found {
		return true
	}
	return v
}

// specInt reads an integer, defaulting when absent.
func specInt(obj *unstructured.Unstructured, fallback int, path ...string) int {
	v, found, _ := unstructured.NestedInt64(obj.Object, append([]string{specRoot}, path...)...)
	if !found {
		return fallback
	}
	return int(v)
}

// specStringMap reads a map of strings.
func specStringMap(obj *unstructured.Unstructured, path ...string) map[string]string {
	v, _, _ := unstructured.NestedStringMap(obj.Object, append([]string{specRoot}, path...)...)
	return v
}

// specStringSlice reads a list of strings.
func specStringSlice(obj *unstructured.Unstructured, path ...string) []string {
	v, _, _ := unstructured.NestedStringSlice(obj.Object, append([]string{specRoot}, path...)...)
	return v
}

// toParams converts a string map into stored settings.
func toParams(in map[string]string) store.Params {
	out := store.Params{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

// reconcileClouds applies every Cloud object.
func (r *Reconciler) reconcileClouds(ctx context.Context) error {
	items, err := r.list(ctx, cloudGVR)
	if err != nil {
		return err
	}
	var errs []error
	for i := range items {
		obj := &items[i]
		err := r.applyCloud(ctx, obj)
		metrics.K8sObject("Cloud", err)
		if err != nil {
			errs = append(errs, err)
			r.setStatus(ctx, cloudGVR, obj, "Error", err.Error(), nil)
		}
	}
	return errors.Join(errs...)
}

func (r *Reconciler) applyCloud(ctx context.Context, obj *unstructured.Unstructured) error {
	name := managedName(obj)
	creds, err := r.secretValues(ctx, obj.GetNamespace(), specString(obj, "secretRef", "name"))
	if err != nil {
		return err
	}

	var c store.Cloud
	tx := r.db.WithContext(ctx).Where("name = ?", name).First(&c)
	if tx.Error != nil {
		c = store.Cloud{Name: name}
	}
	c.Driver = specString(obj, "driver")
	c.Enabled = specBool(obj, "enabled")
	c.Settings = toParams(specStringMap(obj, "settings"))
	c.Settings[managedByLabel] = managedValue
	c.Credentials = store.Secret{}
	maps.Copy(c.Credentials, creds)

	if err := r.db.WithContext(ctx).Save(&c).Error; err != nil {
		return fmt.Errorf("apply cloud %s: %w", name, err)
	}
	if err := r.syncCatalogue(ctx, obj, &c); err != nil {
		return err
	}

	sizes, images := r.catalogueCounts(ctx, c.ID)
	//nolint:gosec // the ids are database primary keys, not attacker input
	r.setStatus(ctx, cloudGVR, obj, "Ready", "", map[string]any{
		"id": int64(c.ID), "sizes": sizes, "images": images,
	})
	return nil
}

// syncCatalogue makes a cloud's sizes and images match the object exactly.
//
// Entries the object no longer lists are removed, because the cluster is the
// source of truth for what it manages — but only when nothing references them,
// since deleting a size out from under a running pool is worse than leaving a
// stale entry behind.
func (r *Reconciler) syncCatalogue(ctx context.Context, obj *unstructured.Unstructured, c *store.Cloud) error {
	wantSizes := map[string]bool{}
	sizes, _, _ := unstructured.NestedSlice(obj.Object, "spec", "sizes")
	for _, raw := range sizes {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		wantSizes[name] = true

		var sz store.Size
		if err := r.db.WithContext(ctx).
			Where("cloud_id = ? AND name = ?", c.ID, name).First(&sz).Error; err != nil {
			sz = store.Size{CloudID: c.ID, Name: name}
		}
		sz.Spec = toParams(stringMapFrom(m["spec"]))
		sz.VCPUs = intFrom(m["vcpus"])
		sz.MemoryMB = intFrom(m["memoryMB"])
		sz.DiskGB = intFrom(m["diskGB"])
		sz.HourlyUSD = floatFrom(m["hourlyUSD"])
		if err := r.db.WithContext(ctx).Save(&sz).Error; err != nil {
			return fmt.Errorf("apply size %s/%s: %w", c.Name, name, err)
		}
	}

	wantImages := map[string]bool{}
	images, _, _ := unstructured.NestedSlice(obj.Object, "spec", "images")
	for _, raw := range images {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		wantImages[name] = true

		var img store.Image
		if err := r.db.WithContext(ctx).
			Where("cloud_id = ? AND name = ?", c.ID, name).First(&img).Error; err != nil {
			img = store.Image{CloudID: c.ID, Name: name}
		}
		img.Spec = toParams(stringMapFrom(m["spec"]))
		img.Username, _ = m["username"].(string)
		img.PreinstalledDocker, _ = m["preinstalledDocker"].(bool)
		if err := r.db.WithContext(ctx).Save(&img).Error; err != nil {
			return fmt.Errorf("apply image %s/%s: %w", c.Name, name, err)
		}
	}

	r.pruneCatalogue(ctx, c.ID, wantSizes, wantImages)
	return nil
}

// pruneCatalogue removes entries the object no longer lists, leaving anything
// a pool still points at.
func (r *Reconciler) pruneCatalogue(ctx context.Context, cloudID uint, keepSizes, keepImages map[string]bool) {
	var sizes []store.Size
	r.db.WithContext(ctx).Where("cloud_id = ?", cloudID).Find(&sizes)
	for i := range sizes {
		if keepSizes[sizes[i].Name] {
			continue
		}
		var used int64
		r.db.WithContext(ctx).Model(&store.Pool{}).Where("size_id = ?", sizes[i].ID).Count(&used)
		if used > 0 {
			r.log.Warn("keeping a size a pool still uses, though the manifest dropped it",
				"size", sizes[i].Name)
			continue
		}
		r.db.WithContext(ctx).Delete(&store.Size{}, sizes[i].ID)
	}

	var images []store.Image
	r.db.WithContext(ctx).Where("cloud_id = ?", cloudID).Find(&images)
	for i := range images {
		if keepImages[images[i].Name] {
			continue
		}
		var used int64
		r.db.WithContext(ctx).Model(&store.Pool{}).Where("image_id = ?", images[i].ID).Count(&used)
		if used > 0 {
			continue
		}
		r.db.WithContext(ctx).Delete(&store.Image{}, images[i].ID)
	}
}

func (r *Reconciler) catalogueCounts(ctx context.Context, cloudID uint) (int64, int64) {
	var sizes, images int64
	r.db.WithContext(ctx).Model(&store.Size{}).Where("cloud_id = ?", cloudID).Count(&sizes)
	r.db.WithContext(ctx).Model(&store.Image{}).Where("cloud_id = ?", cloudID).Count(&images)
	return sizes, images
}

// reconcileForges applies every Forge object.
func (r *Reconciler) reconcileForges(ctx context.Context) error {
	items, err := r.list(ctx, forgeGVR)
	if err != nil {
		return err
	}
	var errs []error
	for i := range items {
		obj := &items[i]
		name := managedName(obj)
		creds, err := r.secretValues(ctx, obj.GetNamespace(), specString(obj, "secretRef", "name"))
		if err != nil {
			errs = append(errs, err)
			r.setStatus(ctx, forgeGVR, obj, "Error", err.Error(), nil)
			continue
		}
		var f store.Forge
		if err := r.db.WithContext(ctx).Where("name = ?", name).First(&f).Error; err != nil {
			f = store.Forge{Name: name}
		}
		f.Kind = specString(obj, "kind")
		f.Enabled = specBool(obj, "enabled")
		f.Settings = toParams(specStringMap(obj, "settings"))
		f.Settings[managedByLabel] = managedValue
		f.Credentials = store.Secret{}
		maps.Copy(f.Credentials, creds)

		saveErr := r.db.WithContext(ctx).Save(&f).Error
		metrics.K8sObject("Forge", saveErr)
		if saveErr != nil {
			errs = append(errs, fmt.Errorf("apply forge %s: %w", name, saveErr))
			r.setStatus(ctx, forgeGVR, obj, "Error", saveErr.Error(), nil)
			continue
		}
		//nolint:gosec // the id is a database primary key, not attacker input
		r.setStatus(ctx, forgeGVR, obj, "Ready", "", map[string]any{"id": int64(f.ID)})
	}
	return errors.Join(errs...)
}

// reconcilePools applies every Pool object.
func (r *Reconciler) reconcilePools(ctx context.Context) error {
	items, err := r.list(ctx, poolGVR)
	if err != nil {
		return err
	}
	var errs []error
	for i := range items {
		obj := &items[i]
		err := r.applyPool(ctx, obj)
		metrics.K8sObject("Pool", err)
		if err != nil {
			errs = append(errs, err)
			r.setStatus(ctx, poolGVR, obj, "Error", err.Error(), nil)
		}
	}
	return errors.Join(errs...)
}

// errUnresolved names what a pool referred to that does not exist yet.
var errUnresolved = errors.New("not found")

func (r *Reconciler) applyPool(ctx context.Context, obj *unstructured.Unstructured) error {
	name := managedName(obj)
	ns := obj.GetNamespace()

	// References are by name, so a manifest can be applied to an empty
	// runnerforge where no ids exist yet.
	forgeName := qualify(ns, specString(obj, "forgeRef"))
	cloudName := qualify(ns, specString(obj, "cloudRef"))

	var f store.Forge
	if err := r.db.WithContext(ctx).Where("name = ?", forgeName).First(&f).Error; err != nil {
		return fmt.Errorf("pool %s: forge %q %w", name, forgeName, errUnresolved)
	}
	var c store.Cloud
	if err := r.db.WithContext(ctx).Where("name = ?", cloudName).First(&c).Error; err != nil {
		return fmt.Errorf("pool %s: cloud %q %w", name, cloudName, errUnresolved)
	}
	var sz store.Size
	sizeName := specString(obj, "size")
	if err := r.db.WithContext(ctx).
		Where("cloud_id = ? AND name = ?", c.ID, sizeName).First(&sz).Error; err != nil {
		return fmt.Errorf("pool %s: size %q on cloud %q %w", name, sizeName, cloudName, errUnresolved)
	}

	var p store.Pool
	if err := r.db.WithContext(ctx).Where("name = ?", name).First(&p).Error; err != nil {
		p = store.Pool{Name: name}
	}
	p.ForgeID, p.CloudID, p.SizeID = f.ID, c.ID, sz.ID
	p.ImageID = nil
	if imageName := specString(obj, "image"); imageName != "" {
		var img store.Image
		if err := r.db.WithContext(ctx).
			Where("cloud_id = ? AND name = ?", c.ID, imageName).First(&img).Error; err != nil {
			return fmt.Errorf("pool %s: image %q on cloud %q %w", name, imageName, cloudName, errUnresolved)
		}
		p.ImageID = &img.ID
	}
	p.Enabled = specBool(obj, "enabled")
	p.Labels = store.StringList(specStringSlice(obj, "labels"))
	p.MinIdle = specInt(obj, 0, "minIdle")
	p.MaxInstances = specInt(obj, defaultMaxInstances, "maxInstances")
	p.JobTimeoutSec = specInt(obj, defaultJobTimeoutSec, "jobTimeoutSeconds")
	p.MaxLifetimeSec = specInt(obj, defaultMaxLifetimeSec, "maxLifetimeSeconds")
	p.ContainerImage = specString(obj, "containerImage")
	p.PublicIPv4 = specBool(obj, "publicIPv4")
	p.AllowSSHFrom = store.StringList(specStringSlice(obj, "allowSSHFrom"))

	// The same rule the UI and the API enforce, so a manifest cannot create a
	// pool whose machines the reaper would destroy mid-job.
	if p.MaxLifetimeSec <= p.JobTimeoutSec {
		return fmt.Errorf("pool %s: maxLifetimeSeconds (%d) must exceed jobTimeoutSeconds (%d)",
			name, p.MaxLifetimeSec, p.JobTimeoutSec)
	}

	p.Forge, p.Cloud, p.Size, p.Image = nil, nil, nil, nil
	if err := r.db.WithContext(ctx).Save(&p).Error; err != nil {
		return fmt.Errorf("apply pool %s: %w", name, err)
	}

	live, _ := r.db.LiveInstances(ctx, p.ID)
	//nolint:gosec // a database primary key
	r.setStatus(ctx, poolGVR, obj, "Ready", "", map[string]any{
		"id": int64(p.ID), "machines": int64(len(live)),
	})
	return nil
}

// qualify resolves a reference within its namespace.
func qualify(ns, name string) string {
	if ns == "" || ns == "default" || strings.Contains(name, "/") {
		return name
	}
	return ns + "/" + name
}

// setStatus writes a resource's status subresource.
//
// Failures are logged rather than returned: not being able to report a status
// must not stop the configuration being applied.
func (r *Reconciler) setStatus(
	ctx context.Context, gvr schema.GroupVersionResource,
	obj *unstructured.Unstructured, phase, message string, extra map[string]any,
) {
	status := map[string]any{
		"phase":              phase,
		"observedGeneration": obj.GetGeneration(),
	}
	if message != "" {
		status["message"] = message
	}
	maps.Copy(status, extra)

	cp := obj.DeepCopy()
	if err := unstructured.SetNestedMap(cp.Object, status, "status"); err != nil {
		r.log.Debug("could not build status", "err", err)
		return
	}
	_, err := r.dyn.Resource(gvr).Namespace(obj.GetNamespace()).
		UpdateStatus(ctx, cp, metav1.UpdateOptions{})
	if err != nil {
		r.log.Debug("could not write status", "resource", obj.GetName(), "err", err)
	}
}

func stringMapFrom(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		if s, ok := val.(string); ok {
			out[k] = s
		}
	}
	return out
}

func intFrom(v any) int {
	switch t := v.(type) {
	case int64:
		return int(t)
	case float64:
		return int(t)
	case int:
		return t
	}
	return 0
}

func floatFrom(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int64:
		return float64(t)
	}
	return 0
}

// pruneDeleted removes records whose cluster object is gone.
//
// Only records this package created are considered: a cloud added through the
// UI must survive a reconcile pass, and a Pool deleted from the cluster must
// not leave a pool running forever. The managed marker is what tells them
// apart.
func (r *Reconciler) pruneDeleted(ctx context.Context) error {
	present := func(gvr schema.GroupVersionResource) (map[string]bool, error) {
		items, err := r.list(ctx, gvr)
		if err != nil {
			return nil, err
		}
		out := map[string]bool{}
		for i := range items {
			out[managedName(&items[i])] = true
		}
		return out, nil
	}

	pools, err := present(poolGVR)
	if err != nil {
		return err
	}
	forges, err := present(forgeGVR)
	if err != nil {
		return err
	}
	clouds, err := present(cloudGVR)
	if err != nil {
		return err
	}

	// Pools first: a cloud cannot be removed while a pool still points at it.
	var allPools []store.Pool
	r.db.WithContext(ctx).Find(&allPools)
	for i := range allPools {
		p := &allPools[i]
		if pools[p.Name] {
			continue
		}
		if !r.managesPool(ctx, p) {
			continue
		}
		live, _ := r.db.LiveInstances(ctx, p.ID)
		if len(live) > 0 {
			// Deleting it would strand the machines, which the reaper finds by
			// the pool name written on them.
			r.log.Warn("pool removed from the cluster still has machines running; "+
				"leaving it until they finish", "pool", p.Name, "machines", len(live))
			continue
		}
		r.log.Info("removing a pool deleted from the cluster", "pool", p.Name)
		r.db.WithContext(ctx).Delete(&store.Pool{}, p.ID)
	}

	r.pruneManaged(ctx, &store.Forge{}, forges, "forge_id")
	r.pruneManaged(ctx, &store.Cloud{}, clouds, "cloud_id")
	return nil
}

// managesPool reports whether a pool came from a cluster object.
//
// A pool carries no settings of its own, so ownership is inherited from the
// cloud it uses: a pool built in the UI on a UI-made cloud is never touched.
func (r *Reconciler) managesPool(ctx context.Context, p *store.Pool) bool {
	var c store.Cloud
	if err := r.db.WithContext(ctx).First(&c, p.CloudID).Error; err != nil {
		return false
	}
	return c.Settings.String(managedByLabel) == managedValue
}

// pruneManaged removes managed clouds or forges no longer present in the
// cluster, leaving any that a pool still references.
func (r *Reconciler) pruneManaged(ctx context.Context, model any, present map[string]bool, column string) {
	switch model.(type) {
	case *store.Forge:
		var rows []store.Forge
		r.db.WithContext(ctx).Find(&rows)
		for i := range rows {
			row := &rows[i]
			if present[row.Name] || row.Settings.String(managedByLabel) != managedValue {
				continue
			}
			if r.stillUsed(ctx, column, row.ID) {
				continue
			}
			r.log.Info("removing a forge deleted from the cluster", "forge", row.Name)
			r.db.WithContext(ctx).Delete(&store.Forge{}, row.ID)
		}
	case *store.Cloud:
		var rows []store.Cloud
		r.db.WithContext(ctx).Find(&rows)
		for i := range rows {
			row := &rows[i]
			if present[row.Name] || row.Settings.String(managedByLabel) != managedValue {
				continue
			}
			if r.stillUsed(ctx, column, row.ID) {
				continue
			}
			r.log.Info("removing a cloud deleted from the cluster", "cloud", row.Name)
			r.db.WithContext(ctx).Delete(&store.Cloud{}, row.ID)
		}
	}
}

// stillUsed reports whether any pool references a record.
func (r *Reconciler) stillUsed(ctx context.Context, column string, id uint) bool {
	var n int64
	r.db.WithContext(ctx).Model(&store.Pool{}).Where(column+" = ?", id).Count(&n)
	return n > 0
}

// Managed reports whether a record came from the cluster, so the UI can show it
// read-only rather than letting an operator edit something that will be
// overwritten on the next pass.
func Managed(settings store.Params) bool {
	return settings.String(managedByLabel) == managedValue
}
