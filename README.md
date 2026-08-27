# runnerforge

Ephemeral CI runners on demand: one throwaway machine per job, for
**GitHub Actions**, **GitLab CI** and **Forgejo Actions**, on any cloud.

Written end to end by Claude for [slop.place](https://slop.place). No human has
reviewed this code line by line.

## What it does

A CI job is queued. runnerforge notices, asks the forge for a credential that is
good for exactly one job, boots a machine carrying that credential, and destroys
the machine when the job ends. Nothing is reused between jobs, so a job cannot
leave anything behind for the next one.

All three forges converged on the same primitive, which is what lets one
controller serve them:

| | credential | machine runs | who deregisters |
|---|---|---|---|
| GitHub | `generate-jitconfig` (1h TTL, always ephemeral) | `run.sh --jitconfig` | GitHub |
| Forgejo | ephemeral runner via API | `forgejo-runner one-job --wait --handle` | Forgejo |
| GitLab | runner authentication token | `run-single --max-builds 1` | runnerforge |

## Design

- **One binary.** Go, SQLite by default (Postgres optional), an embedded HTMX
  web UI. Runs as a container anywhere; Kubernetes is supported, not required.
- **Cloud-agnostic.** Providers implement a five-method interface. A pool asks
  for a size called `large` and an image called `ci-base`; each cloud translates
  those into its own identifiers, so a pool definition moves between clouds
  unchanged.
- **The reaper is the point.** The failure mode of a runner controller is
  leaking machines and billing you for them. runnerforge asks each cloud what it
  is *actually* running rather than trusting its own database, so it can lose
  that database entirely and still clean up.
- **The bill is on the page.** Every machine records what it cost, billed from
  when the provider reported it running rather than from when the row was
  created, because time spent booting is not charged. Machines still running
  show a bill that is still growing.

See [DESIGN.md](DESIGN.md) for the investigation this was built from, including
the per-forge gotchas that shaped it.

## Status

| Component | State |
|---|---|
| **Forgejo** | working, verified end to end against a live instance |
| **GitLab** | working, verified end to end against a live instance |
| **GitHub** | working, verified end to end against a real repository |
| Cloud: OpenStack / OVHcloud | working, verified against real VMs |
| Cloud: Docker (machines-as-containers) | working |
| Controller, reaper, job binding | working |
| Web UI (HTMX) | working |
| Cost tracking | working |
| Hetzner, DigitalOcean, Kubernetes drivers | not started |
| Webhook ingestion (push instead of polling) | not started |

## Testing

Nothing important is mocked. The end-to-end tests run against a real Forgejo
instance and a real Docker daemon; runners are containers rather than cloud VMs,
which is the only substitution.

```sh
go test ./...                          # unit + integration; e2e tests skip themselves

eval "$(testdata/e2e-up.sh)"           # starts Forgejo + a shared docker network
RF_WITH_GITLAB=1 eval "$(testdata/e2e-up.sh)"   # add GitLab (slow, ~4 GiB)
go test ./internal/controller/ -run EndToEnd -v
```

GitHub has no self-hostable substitute, so its end-to-end test needs a real
repository:

```sh
export RF_TEST_GITHUB_TOKEN=$(gh auth token)
export RF_TEST_GITHUB_OWNER=your-org RF_TEST_GITHUB_REPO=your-scratch-repo
```

Every end-to-end test asserts that no machine and no runner registration
survives the run. A test that leaks is a failing test.

180 tests, 85.6% statement coverage, race-clean. The Docker and OpenStack
drivers are tested against stub Engine and Nova/Neutron APIs, so their HTTP
layers are covered without a daemon or a cloud account; the real-cloud
integration test is separate and skips unless `RF_TEST_OS_*` is set.

`golangci-lint run ./...` passes with **every linter enabled** except a
documented deny-list in `.golangci.yml`, each entry with a reason.

## Running it

```sh
docker run -v ./data:/data -p 8080:8080 ghcr.io/slop-place/runnerforge \
  genkey                                    # put the result in runnerforge.yaml
docker run -v ./data:/data -p 8080:8080 ghcr.io/slop-place/runnerforge
```

Then open the UI and add a cloud, a size, an image, a forge and a pool. The
config file holds only identity, database and the encryption key; everything
else is managed in the UI.

The image is 19 MB, runs as a non-root user from `scratch`, and needs no
Kubernetes.
