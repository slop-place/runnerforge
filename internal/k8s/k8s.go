// Package k8s reconciles runnerforge's custom resources into its database.
//
// With this enabled the cluster is the source of truth for anything it manages:
// a Cloud, Forge or Pool object is applied into runnerforge, and the UI shows
// those records as read-only so the two cannot disagree. Anything created
// through the UI or the API is left alone, so the two ways of working coexist
// in one deployment.
package k8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/slop-place/runnerforge/internal/store"
)

// Group and version the custom resources live under.
const (
	Group   = "runnerforge.slop.place"
	Version = "v1alpha1"
)

// The resources this package reconciles.
var (
	cloudGVR = schema.GroupVersionResource{Group: Group, Version: Version, Resource: "clouds"}
	forgeGVR = schema.GroupVersionResource{Group: Group, Version: Version, Resource: "forges"}
	poolGVR  = schema.GroupVersionResource{Group: Group, Version: Version, Resource: "pools"}
)

// managedByLabel marks the records this package owns.
//
// Without it a resource deleted from the cluster would leave its record behind
// forever, and a record created in the UI could be mistaken for an orphan and
// removed. The label is what keeps the two ways of working separate.
const managedByLabel = "runnerforge.slop.place/managed-by"

// managedValue is written into a managed record's settings.
const managedValue = "kubernetes"

// Config configures the reconciler.
type Config struct {
	// Enabled turns the reconciler on. Also settable with the -k8s flag.
	Enabled bool `yaml:"enabled"`

	// Namespace to watch. Empty watches every namespace the credentials allow.
	Namespace string `yaml:"namespace"`
	// Kubeconfig path. Empty uses in-cluster credentials, then the usual
	// default path, which is what makes `runnerforge -k8s` work both inside a
	// cluster and on a laptop pointed at one.
	Kubeconfig string `yaml:"kubeconfig"`
	// Interval between reconcile passes.
	Interval time.Duration `yaml:"interval"`
}

// defaultInterval is how often the cluster is re-read when nothing else
// prompts a pass.
const defaultInterval = 30 * time.Second

// Reconciler applies custom resources into runnerforge's database.
type Reconciler struct {
	dyn  dynamic.Interface
	core kubernetes.Interface
	db   *store.DB
	log  *slog.Logger
	cfg  Config
}

// New builds a Reconciler, returning nil when Kubernetes support is not
// requested so the caller can treat it as optional.
func New(cfg Config, db *store.DB, log *slog.Logger) (*Reconciler, error) {
	restCfg, err := restConfig(cfg.Kubeconfig)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: build dynamic client: %w", err)
	}
	core, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: build client: %w", err)
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	return &Reconciler{dyn: dyn, core: core, db: db, log: log, cfg: cfg}, nil
}

// restConfig resolves cluster credentials: in-cluster first, then a kubeconfig.
func restConfig(path string) (*rest.Config, error) {
	if path == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes: no usable credentials: %w", err)
	}
	return cfg, nil
}

// Run reconciles until the context is cancelled.
func (r *Reconciler) Run(ctx context.Context) error {
	t := time.NewTicker(r.cfg.Interval)
	defer t.Stop()

	r.log.Info("watching Kubernetes for runnerforge resources",
		"namespace", nsLabel(r.cfg.Namespace), "interval", r.cfg.Interval)
	if err := r.Reconcile(ctx); err != nil {
		r.log.Error("initial kubernetes reconcile failed", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("kubernetes reconciler stopped: %w", ctx.Err())
		case <-t.C:
			if err := r.Reconcile(ctx); err != nil {
				r.log.Error("kubernetes reconcile failed", "err", err)
			}
		}
	}
}

func nsLabel(ns string) string {
	if ns == "" {
		return "(all)"
	}
	return ns
}

// Reconcile runs one pass: clouds, then forges, then pools, because a pool
// refers to the other two by name and cannot be resolved before they exist.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	var errs []error
	if err := r.reconcileClouds(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := r.reconcileForges(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := r.reconcilePools(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := r.pruneDeleted(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// list fetches every object of a kind in the configured scope.
func (r *Reconciler) list(ctx context.Context, gvr schema.GroupVersionResource) ([]unstructured.Unstructured, error) {
	var iface dynamic.ResourceInterface = r.dyn.Resource(gvr)
	if r.cfg.Namespace != "" {
		iface = r.dyn.Resource(gvr).Namespace(r.cfg.Namespace)
	}
	out, err := iface.List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The CRDs are not installed. That is a configuration state rather
			// than a failure, so it reads as "nothing to reconcile".
			return nil, nil
		}
		return nil, fmt.Errorf("kubernetes: list %s: %w", gvr.Resource, err)
	}
	return out.Items, nil
}

// secretValues reads the keys of a Secret referenced by a resource.
//
// A resource with no secretRef is normal — a Docker cloud needs no credential —
// so that returns an empty map rather than an error.
func (r *Reconciler) secretValues(ctx context.Context, namespace, name string) (map[string]string, error) {
	if name == "" {
		return map[string]string{}, nil
	}
	sec, err := r.core.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("kubernetes: read secret %s/%s: %w", namespace, name, err)
	}
	out := make(map[string]string, len(sec.Data))
	for k, v := range sec.Data {
		out[k] = string(v)
	}
	return out, nil
}
