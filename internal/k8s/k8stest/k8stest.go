// Package k8stest connects tests to a real Kubernetes cluster.
//
// Nothing here is a fake. The end-to-end tests for the reconciler run against
// an actual API server (kind in CI, or whatever RF_TEST_KUBECONFIG points at),
// because the things worth testing — CRD schema defaults, status subresources,
// Secret resolution, namespace scoping — are all behaviour of the API server
// rather than of the client.
package k8stest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/rand"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// KubeconfigEnv names the variable that points the tests at a cluster.
const KubeconfigEnv = "RF_TEST_KUBECONFIG"

// Group and version the runnerforge resources live under, repeated here so
// this package does not import the reconciler it exists to test.
const (
	Group   = "runnerforge.slop.place"
	Version = "v1alpha1"
)

const (
	// crdFile is where the definitions live, relative to the repository root.
	crdFile = "k8s/crds.yaml"
	// establishTimeout bounds how long a freshly created CRD may take to serve.
	establishTimeout = 60 * time.Second
	// establishPoll is how often that is checked.
	establishPoll = 500 * time.Millisecond
	// nsSuffixLen is the random tail on a test namespace name.
	nsSuffixLen = 6
	// yamlBufferBytes sizes the multi-document decoder.
	yamlBufferBytes = 4096
	// cleanupTimeout bounds the deletion of a test namespace.
	cleanupTimeout = 30 * time.Second
)

var crdGVR = schema.GroupVersionResource{
	Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
}

// Cluster is a connection to the cluster under test.
type Cluster struct {
	// Kubeconfig is the path the connection was built from, so a reconciler
	// can be pointed at the same cluster.
	Kubeconfig string
	Config     *rest.Config
	Dyn        dynamic.Interface
	Core       kubernetes.Interface
}

// Connect returns a connection to the cluster RF_TEST_KUBECONFIG names, or
// skips the test when it is unset.
func Connect(t *testing.T) *Cluster {
	t.Helper()
	path := os.Getenv(KubeconfigEnv)
	if path == "" {
		t.Skipf("%s not set; create a kind cluster (RF_WITH_K8S=1 testdata/e2e-up.sh) "+
			"and point it at the kubeconfig", KubeconfigEnv)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, err := core.Discovery().ServerVersion(); err != nil {
		t.Fatalf("the cluster at %s does not answer: %v", path, err)
	}
	return &Cluster{Kubeconfig: path, Config: cfg, Dyn: dyn, Core: core}
}

// InstallCRDs applies k8s/crds.yaml from the repository and waits until every
// resource it defines is served. Already-installed definitions are updated in
// place, so a test always runs against the schema in the working tree.
func (c *Cluster) InstallCRDs(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	path := repoFile(t, crdFile)
	raw, err := os.ReadFile(path) //nolint:gosec // a path inside the repository
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), yamlBufferBytes)
	var resources []string
	for {
		var obj unstructured.Unstructured
		if err := dec.Decode(&obj); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode %s: %v", path, err)
		}
		if obj.GetKind() != "CustomResourceDefinition" {
			continue
		}
		plural, _, _ := unstructured.NestedString(obj.Object, "spec", "names", "plural")
		resources = append(resources, plural)
		c.upsertCRD(t, ctx, &obj)
	}
	if len(resources) == 0 {
		t.Fatalf("%s defines no CustomResourceDefinition", path)
	}

	// A CRD is created before it is served; listing it answers 404 until the
	// API server has established it.
	deadline := time.Now().Add(establishTimeout)
	for _, res := range resources {
		gvr := schema.GroupVersionResource{Group: Group, Version: Version, Resource: res}
		for {
			_, err := c.Dyn.Resource(gvr).Namespace(metav1.NamespaceDefault).
				List(ctx, metav1.ListOptions{Limit: 1})
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never became served: %v", res, err)
			}
			time.Sleep(establishPoll)
		}
	}
}

// Namespace creates a namespace for one test and removes it afterwards. Every
// object the test makes lives in it, so tests never see each other.
func (c *Cluster) Namespace(t *testing.T) string {
	t.Helper()
	name := "rf-e2e-" + rand.String(nsSuffixLen)
	meta := metav1.ObjectMeta{Name: name}
	ns := &corev1.Namespace{ObjectMeta: meta}
	if _, err := c.Core.CoreV1().Namespaces().Create(t.Context(), ns, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), cleanupTimeout)
		defer cancel()
		err := c.Core.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete namespace %s: %v", name, err)
		}
	})
	return name
}

// Secret creates an opaque Secret.
func (c *Cluster) Secret(t *testing.T, ns, name string, data map[string]string) {
	t.Helper()
	meta := metav1.ObjectMeta{Name: name, Namespace: ns}
	sec := &corev1.Secret{ObjectMeta: meta, StringData: data}
	if _, err := c.Core.CoreV1().Secrets(ns).Create(t.Context(), sec, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create secret %s/%s: %v", ns, name, err)
	}
}

// Create makes a runnerforge object from its spec.
func (c *Cluster) Create(t *testing.T, ns, resource, name string, spec map[string]any) {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": Group + "/" + Version,
		"kind":       kindOf(resource),
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec":       spec,
	}}
	if _, err := c.Dyn.Resource(gvr(resource)).Namespace(ns).Create(t.Context(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create %s %s/%s: %v", resource, ns, name, err)
	}
}

// Update replaces an object's spec, the way `kubectl apply` of an edited
// manifest would.
func (c *Cluster) Update(t *testing.T, ns, resource, name string, spec map[string]any) {
	t.Helper()
	iface := c.Dyn.Resource(gvr(resource)).Namespace(ns)
	obj, err := iface.Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s %s/%s: %v", resource, ns, name, err)
	}
	obj.Object["spec"] = spec
	if _, err := iface.Update(t.Context(), obj, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update %s %s/%s: %v", resource, ns, name, err)
	}
}

// Delete removes an object.
func (c *Cluster) Delete(t *testing.T, ns, resource, name string) {
	t.Helper()
	err := c.Dyn.Resource(gvr(resource)).Namespace(ns).Delete(t.Context(), name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete %s %s/%s: %v", resource, ns, name, err)
	}
}

// Get fetches an object as the API server holds it now.
func (c *Cluster) Get(t *testing.T, ns, resource, name string) *unstructured.Unstructured {
	t.Helper()
	obj, err := c.Dyn.Resource(gvr(resource)).Namespace(ns).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s %s/%s: %v", resource, ns, name, err)
	}
	return obj
}

// Status returns an object's status subresource, empty if nothing has written
// one yet.
func (c *Cluster) Status(t *testing.T, ns, resource, name string) Status {
	t.Helper()
	obj := c.Get(t, ns, resource, name)
	st, _, _ := unstructured.NestedMap(obj.Object, "status")
	return Status{Fields: st, Generation: obj.GetGeneration()}
}

// Status is what the reconciler reported about an object.
type Status struct {
	Fields     map[string]any
	Generation int64
}

// Phase is the reported phase, "" when none.
func (s Status) Phase() string {
	v, _ := s.Fields["phase"].(string)
	return v
}

// Message is the reported message, "" when none.
func (s Status) Message() string {
	v, _ := s.Fields["message"].(string)
	return v
}

// Int reads an integer status field, -1 when absent.
func (s Status) Int(key string) int64 {
	switch v := s.Fields[key].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return -1
}

// Current reports whether the status was written for the object's latest
// generation, which is how a test knows it is not reading a stale pass.
func (s Status) Current() bool {
	return s.Int("observedGeneration") == s.Generation
}

func (c *Cluster) upsertCRD(t *testing.T, ctx context.Context, obj *unstructured.Unstructured) {
	t.Helper()
	iface := c.Dyn.Resource(crdGVR)
	_, err := iface.Create(ctx, obj, metav1.CreateOptions{})
	if err == nil {
		return
	}
	if !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create CRD %s: %v", obj.GetName(), err)
	}
	existing, err := iface.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get CRD %s: %v", obj.GetName(), err)
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	if _, err := iface.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update CRD %s: %v", obj.GetName(), err)
	}
}

// repoFile resolves a path relative to the repository root from wherever the
// test binary runs, which for `go test` is the package directory.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		candidate := filepath.Join(dir, rel)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("%s not found above %s", rel, dir)
		}
		dir = parent
	}
}

func gvr(resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: Group, Version: Version, Resource: resource}
}

// kindOf turns a resource name into its kind: clouds → Cloud.
func kindOf(resource string) string {
	singular := strings.TrimSuffix(resource, "s")
	return fmt.Sprintf("%s%s", strings.ToUpper(singular[:1]), singular[1:])
}
