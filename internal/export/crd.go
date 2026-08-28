package export

import (
	"fmt"
	"strings"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/store"
)

// yamlWriter accumulates a YAML document.
type yamlWriter struct {
	b strings.Builder
	// secrets collects the keys that must come from a Kubernetes Secret, so a
	// companion stub can be emitted alongside the resource.
	secrets []string
	// secretName is the Secret the resource refers to.
	secretName string
}

func (w *yamlWriter) String() string { return w.b.String() + w.stub() }

// CloudCRD renders a cloud, with its sizes and images nested, as a custom
// resource.
//
// Sizes and images are nested rather than being separate kinds because they
// have no meaning apart from their cloud: a size is what "large" means on this
// account, and deleting the account deletes the answer.
func CloudCRD(c *store.Cloud) string {
	name := k8sName(c.Name)
	w := yamlWriter{secretName: "runnerforge-cloud-" + name}

	drv, _ := cloud.DriverByName(c.Driver)
	secrets := secretFields(drv.Schema.Connection)

	w.linef("apiVersion: %s", APIVersion)
	w.linef("kind: Cloud")
	w.linef("metadata:")
	w.linef("  name: %s", name)
	w.linef("spec:")
	w.linef("  driver: %s", yamlValue(c.Driver))
	w.linef("  enabled: %t", c.Enabled)
	w.settings("  ", c.Settings, secrets, c.Credentials)

	w.sizes(c.Sizes)
	w.images(c.Images)
	return w.String()
}

// sizes renders a cloud's size catalogue.
func (w *yamlWriter) sizes(sizes []store.Size) {
	if len(sizes) == 0 {
		return
	}
	w.linef("  sizes:")
	for i := range sizes {
		s := &sizes[i]
		w.linef("    - name: %s", yamlValue(s.Name))
		w.specMap("      ", s.Spec)
		if s.VCPUs > 0 {
			w.linef("      vcpus: %d", s.VCPUs)
		}
		if s.MemoryMB > 0 {
			w.linef("      memoryMB: %d", s.MemoryMB)
		}
		if s.HourlyUSD > 0 {
			w.linef("      hourlyUSD: %s", strconvFloat(s.HourlyUSD))
		}
	}
}

// images renders a cloud's image catalogue.
func (w *yamlWriter) images(images []store.Image) {
	if len(images) == 0 {
		return
	}
	w.linef("  images:")
	for i := range images {
		img := &images[i]
		w.linef("    - name: %s", yamlValue(img.Name))
		w.specMap("      ", img.Spec)
		if img.Username != "" {
			w.linef("      username: %s", yamlValue(img.Username))
		}
		if img.PreinstalledDocker {
			w.linef("      preinstalledDocker: true")
		}
	}
}

// specMap renders a driver spec at the given indent.
func (w *yamlWriter) specMap(indent string, spec store.Params) {
	if len(spec) == 0 {
		return
	}
	w.linef("%sspec:", indent)
	for _, k := range sortedKeys(spec) {
		w.linef("%s  %s: %s", indent, k, yamlValue(spec[k]))
	}
}

func (w *yamlWriter) linef(format string, a ...any) {
	fmt.Fprintf(&w.b, format+"\n", a...)
}

// settings renders a settings map, replacing declared secrets with a reference
// into a Kubernetes Secret. A manifest committed to a repository must not carry
// the credential.
func (w *yamlWriter) settings(
	indent string, s store.Params, secrets map[string]cloud.Field, stored store.Secret,
) {
	keys := sortedKeys(s)
	var secretKeys []string
	for _, k := range sortedKeys(secrets) {
		if stored[k] != "" {
			secretKeys = append(secretKeys, k)
		}
	}
	if len(keys) > 0 {
		w.linef("%ssettings:", indent)
		for _, k := range keys {
			w.linef("%s  %s: %s", indent, k, yamlValue(s[k]))
		}
	}
	if len(secretKeys) == 0 {
		return
	}
	w.secrets = secretKeys
	w.linef("%ssecretRef:", indent)
	w.linef("%s  name: %s", indent, w.secretName)
	w.linef("%s  # keys read from that Secret: %s", indent, strings.Join(secretKeys, ", "))
}

// stub renders a companion Secret with the keys left blank, so applying the
// pair is a matter of filling them in rather than working out what is needed.
func (w *yamlWriter) stub() string {
	if len(w.secrets) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("# Create this Secret with the real values; it is deliberately not\n")
	b.WriteString("# rendered with them, so this file is safe to commit.\n")
	b.WriteString("apiVersion: v1\n")
	b.WriteString("kind: Secret\n")
	b.WriteString("metadata:\n")
	fmt.Fprintf(&b, "  name: %s\n", w.secretName)
	b.WriteString("type: Opaque\n")
	b.WriteString("stringData:\n")
	for _, k := range w.secrets {
		fmt.Fprintf(&b, "  %s: \"\"\n", k)
	}
	return b.String()
}

// ForgeCRD renders a forge connection as a custom resource.
func ForgeCRD(f *store.Forge) string {
	name := k8sName(f.Name)
	w := yamlWriter{secretName: "runnerforge-forge-" + name}
	fields := forgeImpl(f.Kind)

	w.linef("apiVersion: %s", APIVersion)
	w.linef("kind: Forge")
	w.linef("metadata:")
	w.linef("  name: %s", name)
	w.linef("spec:")
	w.linef("  kind: %s", yamlValue(f.Kind))
	w.linef("  enabled: %t", f.Enabled)
	w.settings("  ", f.Settings, secretFields(fields), f.Credentials)
	return w.String()
}

// PoolCRD renders a pool as a custom resource.
//
// It refers to its cloud, size, image and forge by name rather than by id: a
// manifest has to be applicable to an empty runnerforge, where none of those
// ids exist yet.
func PoolCRD(p *store.Pool) string {
	w := yamlWriter{}
	w.linef("apiVersion: %s", APIVersion)
	w.linef("kind: Pool")
	w.linef("metadata:")
	w.linef("  name: %s", k8sName(p.Name))
	w.linef("spec:")
	w.linef("  enabled: %t", p.Enabled)
	if p.Forge != nil {
		w.linef("  forgeRef: %s", yamlValue(k8sName(p.Forge.Name)))
	}
	if p.Cloud != nil {
		w.linef("  cloudRef: %s", yamlValue(k8sName(p.Cloud.Name)))
	}
	if p.Size != nil {
		w.linef("  size: %s", yamlValue(p.Size.Name))
	}
	if p.Image != nil {
		w.linef("  image: %s", yamlValue(p.Image.Name))
	}
	w.linef("  labels:")
	for _, l := range p.Labels {
		w.linef("    - %s", yamlValue(l))
	}
	w.linef("  maxInstances: %d", p.MaxInstances)
	w.linef("  minIdle: %d", p.MinIdle)
	w.linef("  jobTimeoutSeconds: %d", p.JobTimeoutSec)
	w.linef("  maxLifetimeSeconds: %d", p.MaxLifetimeSec)
	if p.ContainerImage != "" {
		w.linef("  containerImage: %s", yamlValue(p.ContainerImage))
	}
	w.linef("  publicIPv4: %t", p.PublicIPv4)
	if len(p.AllowSSHFrom) > 0 {
		w.linef("  allowSSHFrom:")
		for _, c := range p.AllowSSHFrom {
			w.linef("    - %s", yamlValue(c))
		}
	}
	return w.String()
}
