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

See [DESIGN.md](DESIGN.md) for the investigation this was built from, including
the per-forge gotchas that shaped it.

## Status

| Component | State |
|---|---|
| Forgejo | working, verified end to end against a live instance |
| Cloud: Docker (local machines-as-containers) | working |
| Controller, reaper, job binding | working |
| GitLab | in progress |
| GitHub | in progress |
| Cloud: OpenStack / OVHcloud | in progress |
| Web UI | in progress |

## Testing

Nothing important is mocked. The end-to-end tests run against a real Forgejo
instance and a real Docker daemon; runners are containers rather than cloud VMs,
which is the only substitution.

```sh
eval "$(testdata/e2e-up.sh)"   # starts Forgejo + a shared docker network
go test ./...
```

Every end-to-end test asserts that no machine and no runner registration
survives the run. A test that leaks is a failing test.
