package export

import (
	"fmt"
	"strings"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/store"
)

// hclWriter accumulates an HCL document.
type hclWriter struct {
	b strings.Builder
	// vars collects the variable blocks the document needs for its secrets,
	// emitted at the end so the resource reads first.
	vars []string
}

func (w *hclWriter) String() string {
	out := w.b.String()
	if len(w.vars) > 0 {
		out += "\n" + strings.Join(w.vars, "\n\n") + "\n"
	}
	return out
}

func (w *hclWriter) linef(format string, a ...any) {
	fmt.Fprintf(&w.b, format+"\n", a...)
}

// secretVar records a variable for a secret and returns the reference to use.
func (w *hclWriter) secretVar(resource, key, help string) string {
	name := hclIdent(resource + "_" + key)
	desc := help
	if desc == "" {
		desc = "Set this from the environment: TF_VAR_" + name
	}
	w.vars = append(w.vars, fmt.Sprintf(
		"variable %s {\n  type        = string\n  sensitive   = true\n  description = %s\n}",
		hclString(name), hclString(desc)))
	return "var." + name
}

// settingsBlock renders a settings map, substituting a variable reference for
// anything the driver declared secret.
//
// A configuration meant to be committed must not carry the credential, so the
// value is never emitted even though runnerforge holds it.
func (w *hclWriter) settingsBlock(
	indent, resource string, settings store.Params, secrets map[string]cloud.Field, stored store.Secret,
) {
	keys := sortedKeys(settings)
	// Secrets live in a separate column, so they are appended to the same block.
	for _, k := range sortedKeys(secrets) {
		if stored[k] != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return
	}
	w.linef("%ssettings = {", indent)
	for _, k := range keys {
		if f, isSecret := secrets[k]; isSecret {
			w.linef("%s  %-16s = %s", indent, k, w.secretVar(resource, k, f.Help))
			continue
		}
		w.linef("%s  %-16s = %s", indent, k, hclValue(settings[k]))
	}
	w.linef("%s}", indent)
}

// CloudHCL renders a cloud, its sizes and its images as Terraform.
func CloudHCL(c *store.Cloud) string {
	var w hclWriter
	ident := hclIdent(c.Name)

	drv, _ := cloud.DriverByName(c.Driver)
	secrets := secretFields(drv.Schema.Connection)

	w.linef("resource %s %s {", hclString("runnerforge_cloud"), hclString(ident))
	w.linef("  name    = %s", hclString(c.Name))
	w.linef("  driver  = %s", hclString(c.Driver))
	w.linef("  enabled = %t", c.Enabled)
	w.linef("")
	w.settingsBlock("  ", c.Name, c.Settings, secrets, c.Credentials)
	w.linef("}")

	for i := range c.Sizes {
		s := &c.Sizes[i]
		w.linef("")
		w.linef("resource %s %s {", hclString("runnerforge_size"),
			hclString(hclIdent(c.Name+"_"+s.Name)))
		w.linef("  cloud_id = runnerforge_cloud.%s.id", ident)
		w.linef("  name     = %s", hclString(s.Name))
		if len(s.Spec) > 0 {
			w.linef("")
			w.linef("  spec = {")
			for _, k := range sortedKeys(s.Spec) {
				w.linef("    %-16s = %s", k, hclValue(s.Spec[k]))
			}
			w.linef("  }")
		}
		if s.VCPUs > 0 || s.MemoryMB > 0 || s.HourlyUSD > 0 {
			w.linef("")
			if s.VCPUs > 0 {
				w.linef("  vcpus      = %d", s.VCPUs)
			}
			if s.MemoryMB > 0 {
				w.linef("  memory_mb  = %d", s.MemoryMB)
			}
			if s.HourlyUSD > 0 {
				w.linef("  hourly_usd = %s", strconvFloat(s.HourlyUSD))
			}
		}
		w.linef("}")
	}

	for i := range c.Images {
		img := &c.Images[i]
		w.linef("")
		w.linef("resource %s %s {", hclString("runnerforge_image"),
			hclString(hclIdent(c.Name+"_"+img.Name)))
		w.linef("  cloud_id = runnerforge_cloud.%s.id", ident)
		w.linef("  name     = %s", hclString(img.Name))
		if len(img.Spec) > 0 {
			w.linef("")
			w.linef("  spec = {")
			for _, k := range sortedKeys(img.Spec) {
				w.linef("    %-16s = %s", k, hclValue(img.Spec[k]))
			}
			w.linef("  }")
		}
		if img.Username != "" {
			w.linef("")
			w.linef("  username = %s", hclString(img.Username))
		}
		if img.PreinstalledDocker {
			w.linef("  preinstalled_docker = true")
		}
		w.linef("}")
	}

	return w.String()
}

// ForgeHCL renders a forge connection as Terraform.
func ForgeHCL(f *store.Forge) string {
	var w hclWriter
	secrets := secretFields(forgeImpl(f.Kind))

	w.linef("resource %s %s {", hclString("runnerforge_forge"), hclString(hclIdent(f.Name)))
	w.linef("  name    = %s", hclString(f.Name))
	w.linef("  kind    = %s", hclString(f.Kind))
	w.linef("  enabled = %t", f.Enabled)
	w.linef("")
	w.settingsBlock("  ", f.Name, f.Settings, secrets, f.Credentials)
	w.linef("}")
	return w.String()
}

// PoolHCL renders a pool as Terraform, referencing the resources it depends on
// rather than their ids, which is what makes the output usable as written.
func PoolHCL(p *store.Pool) string {
	var w hclWriter

	w.linef("resource %s %s {", hclString("runnerforge_pool"), hclString(hclIdent(p.Name)))
	w.linef("  name    = %s", hclString(p.Name))
	w.linef("  enabled = %t", p.Enabled)
	w.linef("")
	if p.Forge != nil {
		w.linef("  forge_id = runnerforge_forge.%s.id", hclIdent(p.Forge.Name))
	} else {
		w.linef("  forge_id = %d", p.ForgeID)
	}
	if p.Cloud != nil {
		w.linef("  cloud_id = runnerforge_cloud.%s.id", hclIdent(p.Cloud.Name))
	} else {
		w.linef("  cloud_id = %d", p.CloudID)
	}
	if p.Cloud != nil && p.Size != nil {
		w.linef("  size_id  = runnerforge_size.%s.id", hclIdent(p.Cloud.Name+"_"+p.Size.Name))
	} else {
		w.linef("  size_id  = %d", p.SizeID)
	}
	if p.Cloud != nil && p.Image != nil {
		w.linef("  image_id = runnerforge_image.%s.id", hclIdent(p.Cloud.Name+"_"+p.Image.Name))
	} else if p.ImageID != nil {
		w.linef("  image_id = %d", *p.ImageID)
	}
	w.linef("")
	w.linef("  labels = %s", labelsList(p.Labels))
	w.linef("")
	w.linef("  max_instances    = %d", p.MaxInstances)
	w.linef("  min_idle         = %d", p.MinIdle)
	w.linef("  job_timeout_sec  = %d", p.JobTimeoutSec)
	w.linef("  max_lifetime_sec = %d", p.MaxLifetimeSec)
	if p.ContainerImage != "" {
		w.linef("  container_image  = %s", hclString(p.ContainerImage))
	}
	w.linef("  public_ipv4      = %t", p.PublicIPv4)
	if len(p.AllowSSHFrom) > 0 {
		w.linef("  allow_ssh_from   = %s", labelsList(p.AllowSSHFrom))
	}
	w.linef("}")
	return w.String()
}

func strconvFloat(f float64) string {
	return fmt.Sprintf("%g", f)
}
