package export_test

import (
	"strings"
	"testing"

	_ "github.com/slop-place/runnerforge/internal/cloud/dockerdrv"
	_ "github.com/slop-place/runnerforge/internal/cloud/openstack"
	"github.com/slop-place/runnerforge/internal/export"
	_ "github.com/slop-place/runnerforge/internal/forge/forgejo"
	_ "github.com/slop-place/runnerforge/internal/forge/github"
	"github.com/slop-place/runnerforge/internal/store"
)

func ovhCloud() *store.Cloud {
	return &store.Cloud{
		ID: 1, Name: "ovh-us-east", Driver: "openstack", Enabled: true,
		Settings: store.Params{
			"auth_url":   "https://auth.cloud.ovh.us/v3",
			"region":     "US-EAST-VA-1",
			"project_id": "07c644",
		},
		// A stored secret must never reach the rendered output.
		Credentials: store.Secret{"username": "user-xyz", "password": "hunter2"},
		Sizes: []store.Size{{
			ID: 1, CloudID: 1, Name: "large",
			Spec: store.Params{"flavor": "c3-8"}, VCPUs: 4, MemoryMB: 8192, HourlyUSD: 0.074,
		}},
		Images: []store.Image{{
			ID: 1, CloudID: 1, Name: "ci-base",
			Spec: store.Params{"id": "22ef146b"}, Username: "debian", PreinstalledDocker: true,
		}},
	}
}

func TestCloudHCLNeverRendersSecrets(t *testing.T) {
	t.Parallel()
	out := export.CloudHCL(ovhCloud())

	// This is the property that makes the output safe to commit.
	for _, secret := range []string{"hunter2", "user-xyz"} {
		if strings.Contains(out, secret) {
			t.Fatalf("the rendered HCL contains a stored secret (%q):\n%s", secret, out)
		}
	}
	// It has to reference them, or the configuration would be incomplete.
	if !strings.Contains(out, "var.ovh_us_east_password") {
		t.Errorf("no variable reference for the password:\n%s", out)
	}
	if !strings.Contains(out, `variable "ovh_us_east_password"`) {
		t.Errorf("no variable block was declared:\n%s", out)
	}
	if !strings.Contains(out, "sensitive   = true") {
		t.Error("the secret variable is not marked sensitive")
	}
}

func TestCloudHCLShape(t *testing.T) {
	t.Parallel()
	out := export.CloudHCL(ovhCloud())

	for _, want := range []string{
		`resource "runnerforge_cloud" "ovh_us_east"`,
		`driver  = "openstack"`,
		`region           = "US-EAST-VA-1"`,
		`resource "runnerforge_size" "ovh_us_east_large"`,
		// Sizes and images must reference the cloud rather than hard-code an
		// id, or the output cannot be applied to an empty deployment.
		"cloud_id = runnerforge_cloud.ovh_us_east.id",
		`flavor           = "c3-8"`,
		"hourly_usd = 0.074",
		`resource "runnerforge_image" "ovh_us_east_ci_base"`,
		"preinstalled_docker = true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("HCL is missing %q:\n%s", want, out)
		}
	}
}

func TestPoolHCLReferencesItsDependencies(t *testing.T) {
	t.Parallel()
	c := ovhCloud()
	imageID := uint(1)
	p := &store.Pool{
		ID: 1, Name: "github-large", Enabled: true,
		ForgeID: 1, CloudID: 1, SizeID: 1, ImageID: &imageID,
		Forge: &store.Forge{ID: 1, Name: "github-main", Kind: "github"},
		Cloud: c, Size: &c.Sizes[0], Image: &c.Images[0],
		Labels:       store.StringList{"self-hosted", "linux"},
		MaxInstances: 10, JobTimeoutSec: 3600, MaxLifetimeSec: 7200, PublicIPv4: true,
	}
	out := export.PoolHCL(p)

	// References, not ids: an id from one deployment means nothing in another.
	for _, want := range []string{
		"forge_id = runnerforge_forge.github_main.id",
		"cloud_id = runnerforge_cloud.ovh_us_east.id",
		"size_id  = runnerforge_size.ovh_us_east_large.id",
		"image_id = runnerforge_image.ovh_us_east_ci_base.id",
		`labels = ["self-hosted", "linux"]`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pool HCL is missing %q:\n%s", want, out)
		}
	}
}

func TestPoolHCLFallsBackToIDs(t *testing.T) {
	t.Parallel()
	// With nothing preloaded the renderer still produces something applyable,
	// rather than emitting a reference to a resource that is not in the file.
	out := export.PoolHCL(&store.Pool{
		Name: "p", ForgeID: 3, CloudID: 4, SizeID: 5,
		Labels: store.StringList{"linux"}, MaxInstances: 2,
	})
	if !strings.Contains(out, "forge_id = 3") || !strings.Contains(out, "size_id  = 5") {
		t.Errorf("expected numeric fallbacks:\n%s", out)
	}
}

func TestCloudCRDNeverRendersSecrets(t *testing.T) {
	t.Parallel()
	out := export.CloudCRD(ovhCloud())

	for _, secret := range []string{"hunter2", "user-xyz"} {
		if strings.Contains(out, secret) {
			t.Fatalf("the rendered CRD contains a stored secret (%q):\n%s", secret, out)
		}
	}
	// It points at a Secret and ships a stub, so applying the pair is a matter
	// of filling in values rather than working out what is needed.
	if !strings.Contains(out, "secretRef:") {
		t.Errorf("no secretRef:\n%s", out)
	}
	if !strings.Contains(out, "kind: Secret") {
		t.Errorf("no companion Secret stub:\n%s", out)
	}
	if !strings.Contains(out, "password: \"\"") {
		t.Errorf("the stub does not list the keys to fill in:\n%s", out)
	}
}

func TestCloudCRDShape(t *testing.T) {
	t.Parallel()
	out := export.CloudCRD(ovhCloud())
	for _, want := range []string{
		"apiVersion: " + export.APIVersion,
		"kind: Cloud",
		"name: ovh-us-east",
		"driver: openstack",
		"sizes:",
		"- name: large",
		"flavor: c3-8",
		"images:",
		"preinstalledDocker: true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("CRD is missing %q:\n%s", want, out)
		}
	}
}

func TestPoolCRDReferencesByName(t *testing.T) {
	t.Parallel()
	c := ovhCloud()
	p := &store.Pool{
		Name: "github-large", Enabled: true,
		Forge: &store.Forge{Name: "github-main", Kind: "github"},
		Cloud: c, Size: &c.Sizes[0],
		Labels: store.StringList{"self-hosted"}, MaxInstances: 10,
		JobTimeoutSec: 3600, MaxLifetimeSec: 7200,
	}
	out := export.PoolCRD(p)
	// By name, not id: a manifest has to apply to an empty runnerforge where
	// no ids exist yet.
	if !strings.Contains(out, "forgeRef: github-main") {
		t.Errorf("expected a forgeRef by name:\n%s", out)
	}
	if !strings.Contains(out, "cloudRef: ovh-us-east") {
		t.Errorf("expected a cloudRef by name:\n%s", out)
	}
	if !strings.Contains(out, "size: large") {
		t.Errorf("expected the size by name:\n%s", out)
	}
}

func TestNamesAreSanitised(t *testing.T) {
	t.Parallel()
	c := &store.Cloud{Name: "OVH US East (prod!)", Driver: "docker", Enabled: true}

	hcl := export.CloudHCL(c)
	// Terraform labels cannot carry spaces or punctuation.
	if !strings.Contains(hcl, `"ovh_us_east_prod"`) {
		t.Errorf("HCL identifier was not sanitised:\n%s", hcl)
	}
	crd := export.CloudCRD(c)
	// Kubernetes object names must be DNS-1123 subdomains.
	if !strings.Contains(crd, "name: ovh-us-east-prod") {
		t.Errorf("Kubernetes name was not sanitised:\n%s", crd)
	}
}

func TestValuesAreQuotedSafely(t *testing.T) {
	t.Parallel()
	c := &store.Cloud{
		Name: "c", Driver: "docker", Enabled: true,
		Settings: store.Params{
			"quote":  `a "quoted" value`,
			"interp": "${not_a_variable}",
			"yesish": "yes",
			"numish": "0755",
		},
	}
	hcl := export.CloudHCL(c)
	// An unescaped ${...} would become a Terraform interpolation.
	if !strings.Contains(hcl, `$${not_a_variable}`) {
		t.Errorf("interpolation was not escaped:\n%s", hcl)
	}
	if !strings.Contains(hcl, `a \"quoted\" value`) {
		t.Errorf("quotes were not escaped:\n%s", hcl)
	}

	crd := export.CloudCRD(c)
	// Unquoted, YAML would read these as a boolean and a number.
	if !strings.Contains(crd, `yesish: "yes"`) {
		t.Errorf("a YAML-ambiguous boolean was left unquoted:\n%s", crd)
	}
	if !strings.Contains(crd, `numish: "0755"`) {
		t.Errorf("a YAML-ambiguous number was left unquoted:\n%s", crd)
	}
}

func TestRenderingIsStable(t *testing.T) {
	t.Parallel()
	// The same configuration must render identically every time, or a diff
	// against a committed file would mean nothing.
	c := ovhCloud()
	for range 10 {
		if export.CloudHCL(c) != export.CloudHCL(ovhCloud()) {
			t.Fatal("HCL rendering is not stable across calls")
		}
		if export.CloudCRD(c) != export.CloudCRD(ovhCloud()) {
			t.Fatal("CRD rendering is not stable across calls")
		}
	}
}

func TestForgeRendering(t *testing.T) {
	t.Parallel()
	f := &store.Forge{
		ID: 1, Name: "github-main", Kind: "github", Enabled: true,
		Settings:    store.Params{"owner": "slop-place", "repo": "ci", "scope": "repo"},
		Credentials: store.Secret{"token": "ghp_secret"},
	}
	hcl := export.ForgeHCL(f)
	if strings.Contains(hcl, "ghp_secret") {
		t.Error("the forge token was rendered into HCL")
	}
	if !strings.Contains(hcl, "var.github_main_token") {
		t.Errorf("no variable reference for the token:\n%s", hcl)
	}
	crd := export.ForgeCRD(f)
	if strings.Contains(crd, "ghp_secret") {
		t.Error("the forge token was rendered into the CRD")
	}
	if !strings.Contains(crd, "kind: Forge") {
		t.Errorf("wrong kind:\n%s", crd)
	}
}
