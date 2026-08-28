# terraform-provider-runnerforge

Manage a [runnerforge](https://github.com/slop-place/runnerforge) deployment as
code: the clouds it provisions machines on, the forges it serves, and the pools
that connect them.

```hcl
terraform {
  required_providers {
    runnerforge = { source = "slop-place/runnerforge" }
  }
}

provider "runnerforge" {
  endpoint = "https://runnerforge.example.com"
  # token comes from RUNNERFORGE_TOKEN; a token in a .tf file ends up in
  # version control.
}

resource "runnerforge_cloud" "ovh" {
  name   = "ovh-us-east"
  driver = "openstack"
  settings = {
    auth_url   = "https://auth.cloud.ovh.us/v3"
    region     = "US-EAST-VA-1"
    project_id = var.ovh_project_id
    username   = var.ovh_username
    password   = var.ovh_password
  }
}

resource "runnerforge_size" "large" {
  cloud_id   = runnerforge_cloud.ovh.id
  name       = "large"
  hourly_usd = 0.0740
  spec       = { flavor = "c3-8" }
}

resource "runnerforge_pool" "ci" {
  name     = "github-large"
  forge_id = runnerforge_forge.github.id
  cloud_id = runnerforge_cloud.ovh.id
  size_id  = runnerforge_size.large.id
  labels   = ["self-hosted", "linux", "x64"]

  max_instances    = 10
  job_timeout_sec  = 3600
  max_lifetime_sec = 7200
}
```

## Resources

| Resource | What it is |
|---|---|
| `runnerforge_cloud` | A provider account machines are created on |
| `runnerforge_size` | One entry in a cloud's size catalogue |
| `runnerforge_image` | One entry in a cloud's image catalogue |
| `runnerforge_forge` | A connection to GitHub, GitLab or Forgejo |
| `runnerforge_pool` | One forge, one cloud, one machine shape |

## You do not have to write this by hand

Every runnerforge deployment renders its own configuration. Configure a pool
through the UI, press **As Terraform**, and paste the result — it references the
other resources properly rather than hard-coding ids, and leaves secrets as
`var.` references so the output is safe to commit.

## Notes

**Which settings a driver takes** depends on the driver, and the same is true of
forges. The deployment's UI lists them with help text; the export button renders
a block that already has them right.

**Secrets are never read back.** runnerforge stores them encrypted and does not
return them, so `terraform plan` will not show a diff for a credential once it
is set — and will not propose erasing one either.

## Development

```sh
go build -o ~/go/bin/terraform-provider-runnerforge .
```

Then point Terraform at the local binary with a `dev_overrides` block in
`~/.terraformrc`.
