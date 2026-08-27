# ovhcloud-ci-runner — design investigation

A Kubernetes-native controller that provisions **one throwaway OVHcloud VM per CI job** for
GitHub Actions, GitLab CI and Forgejo Actions.

Status: investigation / pre-implementation. Nothing built yet.

---

## 0. Decisions taken

| Question | Decision | Consequence |
|---|---|---|
| Deployment | **Any container host.** Kubernetes is supported, not required — state lives in SQLite or Postgres, not etcd. | Runner VMs get public IPv4; that cost stays in the model. Private-network/gateway mode becomes a later per-pool option, not the default. GitHub webhooks need a publicly reachable ingress. |
| GitLab path | **Changed during implementation** — shipped as the unified `run-single` path, not the fleeting plugin. See §3 for why. | No SSH into runner machines, so GitLab works on container-mode clouds and is testable without spending money. |
| Job execution | **Docker on the VM** | Golden image ships Docker + prewarmed base images. See §4.1 for what "in a container" actually means per forge. |
| Forges | **GitHub.com**, **self-managed GitLab**, **self-hosted Forgejo** | GitHub App auth + public webhook ingress; GitLab and Forgejo reached over your network — the manager *and* the runner VMs must both be able to reach them. |

### Fallout worth naming now

- **GitHub.com webhooks need a public endpoint.** With the cluster "anywhere", that's an Ingress
  with a real hostname + TLS. If that's unacceptable, the fallback is polling
  `GET /orgs/{org}/actions/runs?status=queued`, which is slower and burns rate limit. Use
  `smee.io` or `gh webhook forward` for local development only.
- **Self-hosted GitLab and Forgejo must be reachable from OVHcloud VMs**, not just from the
  cluster — the runner on the VM talks directly to the forge. If they're behind a VPN, that
  changes the network design substantially (VPN client in the golden image, or an OVH-side
  gateway). **Confirm this before milestone 2.**
- **SSH source restriction for the GitLab pool.** Restricting inbound 22 to the manager needs a
  *stable* egress IP from the cluster. Arbitrary clusters often don't have one. Options, in
  order of preference: (a) a NAT gateway / static egress IP for the manager pods; (b) leave 22
  open to the world with a per-VM ephemeral keypair, key-only auth, no password, and a VM
  lifetime measured in minutes. (b) is defensible given the VM's blast radius is one job, but it
  should be a conscious choice.

---

## 1. The unifying insight

All three forges independently converged on the same primitive:

> ask the forge for a **single-use runner credential** → boot a VM whose cloud-init carries it →
> the runner process picks up **exactly one job** → the forge deregisters the runner → we delete the VM.

That means one controller with a small `Forge` interface, and one OVHcloud provisioning driver
shared by all of them.

| | GitHub Actions | Forgejo Actions | GitLab CI |
|---|---|---|---|
| Single-use credential | `POST /orgs/{org}/actions/runners/generate-jitconfig` → `encoded_jit_config` (TTL 1h, always ephemeral) | `POST /api/v1/admin/actions/runners` `{"ephemeral": true}` → `{id, uuid, token}` | runner authentication token (not single-use) |
| On-VM command | `./run.sh --jitconfig <b64>` | `forgejo-runner one-job --uuid … --token … --label … --handle --wait` | `gitlab-runner run-single --max-builds 1 --wait-timeout N` |
| Auto-deregister | yes (JIT runners are one-shot) | yes (Forgejo enforces ephemeral, cannot be bypassed) | no — we delete |
| Demand signal | `workflow_job` **webhook** (`queued`) — push, low latency | poll `GET /api/v1/{scope}/actions/runners/jobs` | see §3 |
| Auth for control plane | GitHub App installation token (`admin:org` / "Self-hosted runners") | admin API token | PAT / group token |

### Forge-specific gotchas found

- **GitHub**: JIT config is base64 and contains the credential — it goes in `userData`, which is
  readable from the instance's metadata service. Acceptable (single-use, 1h TTL), but nothing
  else may share that channel. If a VM dies before claiming a job the runner is orphaned in the
  org list → reaper must `DELETE /orgs/{org}/actions/runners/{id}`.
- **Forgejo**: `one-job` is the *only* command supporting ephemeral mode (`daemon` refuses).
  `--handle` is mandatory for correct label matching — **and every runner in the same scope must
  also use `--handle`**, otherwise jobs land on incompatible runners. Waiting jobs reported by the
  API may be blocked by concurrency groups, so "waiting" ≠ "schedulable"; `--wait` on the VM
  absorbs that ambiguity at the cost of paying for an idle VM. Needs Forgejo ≥ v11 and
  forgejo-runner ≥ 6.1.
- **GitLab**: the odd one out — no per-job credential, and historically no reliable webhook when a
  job enters `pending`. Reimplementing `POST /api/v4/jobs/request` to detect demand is not an
  option: polling that endpoint *consumes* the job.

## 2. OVHcloud: which API

Two options, both usable, not equivalent.

**OVHcloud APIv6** — `POST /cloud/project/{sid}/region/{region}/instance` (`cloud.instance.CreateInput`).
Verified against the live schema (`https://eu.api.ovh.com/1.0/cloud.json`): supports
`userData`, `bulk`, `bootFrom`, `flavor`, `network`, `group` (affinity/anti-affinity),
`sshKey`/`sshKeyCreate`, `billingPeriod`. Auth is application key/secret + consumer key.
**No security-group and no instance-metadata (tag) endpoints.**

**OpenStack** — Keystone v3 at `https://auth.cloud.ovh.net/v3/`, then Nova/Neutron/Glance.
Service user is created through APIv6: `POST /cloud/project/{sid}/user` (+ `/role`), then
`GET /cloud/project/{sid}/user/{id}/openrc` or `POST …/token`. Gives security groups, server
groups, **server metadata** (essential for ownership tagging and orphan detection), and snapshots.

**Recommendation: OpenStack (gophercloud) as the primary driver**; APIv6 only for what OpenStack
can't do — project quota introspection, creating the service user, billing/usage. Bonus: the
driver then works on any OpenStack cloud, and lets us reuse/fork
[`fleeting-plugin-openstack`](https://github.com/sardinasystems/fleeting-plugin-openstack) (Apache-2.0).

### Billing facts that make per-job VMs viable
- Metered **per second after a 1-minute minimum**.
- The meter starts at `ACTIVE` — time spent in `BUILD` is **not billed**.
- Post-Oct-2026 pricing: on gen-3 (b3/c3/r3), local storage **and the public IPv4** are billed
  separately from the instance. So cost/job = flavor + IPv4 + disk. Runners only need *egress*,
  so a private network + OVH public gateway removes the per-VM IPv4 charge — worth it above a
  threshold of concurrent VMs, and it also removes all inbound exposure (see §3 caveat).

### Latency is the real constraint
VM boot + cloud-init + image pulls dominates. Mitigations, in order of payoff:
1. **Golden image** built with Packer (openstack builder): runner binary, Docker, common base
   images pre-pulled. Rebuild nightly.
2. **Warm pool** — keep `minIdle` VMs at `ACTIVE` waiting on `--wait` / a listener. Costs money;
   make it per-pool and schedule-aware (business hours).
3. Anti-affinity via instance groups so a hypervisor incident doesn't take out a whole pool.

---

## 2a. The cloud abstraction

The cloud layer is a first-class interface, not an OVH client with an interface bolted on. The
only reliable way to keep it honest is to have **two drivers from day one** — if OVH is the sole
implementation, OVH assumptions leak into the controller within a week.

```go
type Provider interface {
    // Capabilities is queried by the controller at startup; pools that ask for
    // something the driver can't do fail validation instead of failing at runtime.
    Capabilities() Capabilities

    // Provision returns as soon as the create call is accepted. It does NOT wait
    // for the instance to become ready — the controller polls Get.
    Provision(ctx context.Context, req ProvisionRequest) (*Instance, error)
    Get(ctx context.Context, id string) (*Instance, error)
    Delete(ctx context.Context, id string) error

    // List is the reaper's ground truth: every instance this driver believes it
    // owns, whether or not the controller knows about it. Must not be cached.
    List(ctx context.Context, owner OwnerTag) ([]*Instance, error)
}

type ProvisionRequest struct {
    Name     string          // ci-<pool>-<ulid>
    Owner    OwnerTag        // controller identity + pool; how the reaper claims instances
    Image    ImageRef        // resolved per-provider from a pool's abstract image name
    Size     SizeRef         // abstract class e.g. "cpu-4-8", mapped per provider in config
    UserData []byte          // cloud-init; carries the single-use runner credential
    Network  NetworkSpec     // PublicIPv4 | PrivateOnly, ingress rules
    SSHKey   *PublicKey      // only populated for the GitLab/docker-autoscaler path
    Labels   map[string]string
}

type Capabilities struct {
    Tags              bool // server metadata for ownership; see the OVH APIv6 trap below
    SecurityGroups    bool
    PrivateNetworking bool
    Spot              bool
    BillingGranularity time.Duration
    TypicalBootTime    time.Duration // informs the controller's readiness timeout
}
```

**Why `Capabilities.Tags` is not academic.** The reaper claims instances by an owner tag. OVH
APIv6 has no server-metadata endpoint, so a hypothetical APIv6-based driver could not implement
`List(owner)` correctly and would have to fall back to name-prefix matching. That single fact is
most of the argument for building the OVH driver on OpenStack/Nova (§2). Any future driver that
can't tag instances must declare it, and the controller then refuses to run a pool on it unless
`allowNamePrefixOwnership: true` is set explicitly.

**Abstract sizes and images.** A `RunnerPool` says `size: cpu-4-8` and `image: ci-base`, never
`b3-8` or a Glance UUID. The provider config carries the mapping. This is what makes a pool
portable across clouds and keeps cloud-specific identifiers out of the user-facing API.

### Planned drivers

| Driver | Purpose | Status |
|---|---|---|
| `docker` | Provisions **local containers** instead of VMs. Exists to run the full controller loop in CI with no cloud spend, and to keep the abstraction honest. | build first |
| `ovh` | OpenStack/gophercloud against OVHcloud (§2). | build second |
| `openstack` | Generic; likely just `ovh` with the OVH-specific bootstrap removed. | near-free once `ovh` exists |
| `hetzner`, `scaleway` | Later. Both have simple flat APIs and per-second-ish billing; good abstraction stress tests. | later |

The `docker` driver is not a mock. It implements the same interface with real create/get/delete/
list semantics and real cloud-init handling (via a small init wrapper), so an e2e test that
passes on `docker` exercises every line of the controller except the OVH API calls themselves.


## 3. GitLab needs a different approach — and the decision changed

Two designs were considered, sharing the OVH driver but differing above it.

**(A) Fleeting plugin + `docker-autoscaler`.** Ship `fleeting-plugin-ovh` and
have the controller reconcile a `gitlab-runner` Deployment per pool. GitLab's own
taskscaler owns the queue, so the demand-signal problem disappears.

**(B) Unified path.** Poll for pending jobs, boot a machine running
`gitlab-runner run-single --max-builds 1`, exactly like the other two forges.

**This was originally decided as (A), and shipped as (B).** Two things changed
after that decision, both of which invalidated its premises:

1. **State moved to a database and the UI.** The original design had the
   controller reconciling Kubernetes Deployments. It no longer reconciles
   anything Kubernetes-shaped, so "manage a `gitlab-runner` Deployment per pool"
   stopped being a natural fit and became a special case.
2. **Container-mode clouds became first-class.** Path A requires the controller
   to hold **SSH access into every runner machine**. That cannot work on the
   `docker` driver or a future Kubernetes driver, where a runner is a container
   with no sshd — which would have left GitLab as the one forge that could not
   be tested without spending money, and the one that could not run on half the
   providers.

Path B costs a polling loop and slightly loose tag matching. Path A would have
cost SSH into every machine and a permanent structural exception. B is the
better trade, and it is what the end-to-end test proves works.

Path A remains viable as a second provisioning path for operators who want
GitLab's native queue semantics; the `Forge` interface still expresses it.

### What GitLab still costs us

GitLab has no per-job credential and never deletes runners itself, so
runnerforge does two things the forge does not:

- **single-use is enforced on our side**, with `--max-builds 1` on the machine
  and by destroying the machine afterwards;
- **the registration is deleted by the reaper**, because a registration left
  behind is a token that outlives the machine it was minted for.

## 4. Architecture (Kubernetes-native)

Go + kubebuilder/controller-runtime. Single manager container, leader election, etcd as the only
state store — no external database.

### 4.1 What "jobs run in Docker" means per forge

The three forges differ here, and the golden image has to satisfy all of them:

- **Forgejo** — `act_runner` runs every job step inside a container by default (docker backend).
  The VM needs a working Docker daemon and the runner talks to `/var/run/docker.sock`.
- **GitLab** — `docker-autoscaler` wraps the Docker executor, so jobs are *always* containers.
  The manager SSHes in and drives the remote Docker daemon.
- **GitHub** — steps run on the **host** unless the workflow declares `container:`. Docker is
  needed for `container:`, `services:`, container actions and `docker build`. So the host
  filesystem *is* the job environment for most GitHub workflows — which is fine precisely
  because the VM is destroyed afterwards.

Consequence for the image: one Packer-built image with Docker, the three runner binaries, and a
prewarmed image cache; `userData` selects which runner to launch. Keep it a single image per
(region, architecture) rather than one per forge — fewer rebuilds, better cache hit rate.

### CRDs
- **`ForgeConnection`** — forge type (`github|gitlab|forgejo`), base URL, scope (org/repo/group/
  instance), `secretRef` to credentials. Supports GHES / self-managed GitLab / self-hosted Forgejo.
- **`RunnerPool`** — the user-facing object: forge ref, runner labels/tags, OVH region + flavor +
  image, `minIdle`/`maxInstances`, job timeout, max VM lifetime, network mode.
- **`RunnerInstance`** — internal, one per VM. Status carries OVH instance ID, forge runner ID and
  a phase machine: `Pending → Provisioning → Booting → Registered → Busy → Terminating → Gone`.
  Crash-safe by construction; a controller restart re-reconciles from etcd + OVH + forge state.

### Components in the manager pod
- **Webhook receiver** (Service + Ingress) for GitHub `workflow_job`; HMAC-verified, writes
  `RunnerInstance` objects. Idempotent on `workflow_job.id`.
- **Pollers** for Forgejo (and GitLab if path B), with jitter and per-forge rate-limit budgets.
- **VM lifecycle reconciler** — the only thing that talks to OVH.
- **Reaper** — every N minutes, list OVH servers by metadata tag `ci-runner-pool=<pool>`, and
  cross-check against `RunnerInstance` + the forge's runner list. Deletes: VMs past
  `maxLifetime`, VMs with no owning CR, forge runners with no VM. This is the component that
  decides whether the whole thing leaks money; write it first, not last.
- **Metrics** — queue depth, time-to-`ACTIVE`, time-to-first-job, VM-seconds and estimated €/job,
  orphan count, forge API budget remaining.

### Security posture
Runner VMs execute untrusted PR code. Non-negotiables:
- one job per VM, VM destroyed after, never reused;
- **no OVH API credentials on the VM** — the VM never talks to the control plane;
- egress-only security group; block the OpenStack metadata service after cloud-init completes so
  the job can't re-read the (already-spent) runner token;
- forge credentials only in k8s Secrets (External Secrets-compatible), GitHub App over PAT;
- for GitLab path A, inbound 22 restricted to the manager's source IP with an ephemeral keypair
  generated per VM.

## 5. Prior art worth reading before writing code
- `actions/actions-runner-controller` — CRD shape and the listener/webhook split.
- `machulav/ec2-github-runner`, `louisgundelwein/runner-autoscaler` (Hetzner) — minimal JIT flows.
- `sardinasystems/fleeting-plugin-openstack`, `cloudscale-ch/fleeting-plugin-cloudscale` — plugin skeleton.
- `aahlenst.dev/blog/autoscaling-forgejo-runner` — the `--handle` trap.

## 6. Remaining open questions

1. **Are the self-hosted GitLab and Forgejo reachable from the public internet?** Runner VMs, not
   just the controller, must reach them. Blocks milestone 4/5 if the answer is "VPN only".
2. Target concurrency and acceptable cold-start latency — decides whether warm pools are needed
   at all, and how much idle spend is tolerable.
3. Which OVH regions and flavors? (GRA/SBG/BHS differ in availability; b3 vs c3 changes €/job.)
4. Does the manager have a stable egress IP available (see §0 fallout, SSH restriction)?
5. Are GPU jobs in scope? Changes flavor quotas and image build.

## 7. Suggested milestones
1. OVH driver (OpenStack) + `RunnerInstance` CRD + reaper — provable "boot and reliably destroy".
2. GitHub path end-to-end (webhook → jitconfig → VM → job → destroy).
3. Golden image pipeline (Packer) + warm pool.
4. Forgejo path (poller + ephemeral registration + `one-job --handle`).
5. GitLab path A (fleeting plugin + managed `gitlab-runner` Deployment).
6. Cost/metrics dashboard, Helm chart.

---

## 8. Autonomous development plan

This project is developed end to end by Claude for [slop.place](https://slop.place). The
constraint that shapes everything below: **I can only work autonomously on the parts where I can
close a feedback loop without a human.** So the architecture is deliberately arranged to maximise
that surface.

### The three self-hostable dependencies

The reason a large majority of this project is autonomously developable is that the interesting
dependencies all run locally in Docker:

| Dependency | Local substitute | Fidelity |
|---|---|---|
| Forgejo | `codeberg.org/forgejo/forgejo` container | **Real.** Real repos, real workflows, real ephemeral runner registration, real `one-job` protocol. |
| GitLab | `gitlab/gitlab-ce` container (~4 GiB RAM) | **Real.** And self-managed GitLab is the actual target, so this *is* production. |
| Cloud | `docker` provider (§2a) | Real interface semantics, fake compute. Covers everything but the OVH API calls. |
| Kubernetes | `kind` | Real API server, real CRDs, real controller-runtime. |
| GitHub | — | **No local substitute.** See below. |

### What I can do with no input at all

Everything except the two items in the next section:

- repo scaffold, Go module, kubebuilder CRDs, controller-runtime wiring, Helm chart;
- the `Provider` interface and the `docker` driver;
- the `Forge` interface and all three clients;
- the **reaper**, tested by deliberately leaking instances and asserting they get collected;
- the OVH driver, written against the published OpenStack/gophercloud API and unit-tested with a
  recorded-HTTP fixture suite — writable and reviewable without credentials, just not *verified*;
- `fleeting-plugin-ovh`, tested against the `docker` provider through gitlab-runner's plugin contract;
- the Packer template (written, not built);
- unit tests, `envtest` controller tests, and a `make e2e-local` that stands up kind + Forgejo +
  the `docker` provider, pushes a real workflow, and asserts the job ran **and** nothing leaked.

Forgejo and GitLab therefore reach "done and verified" with zero human involvement.

### What I cannot verify alone

1. **Real OVHcloud behaviour.** Boot latency, quota errors under concurrency, region
   availability, actual €/job, and whether the reaper holds up against real API flakiness. The
   code can be written blind; it cannot be trusted blind.
2. **GitHub.com.** No self-hostable substitute exists. Repo-level `generate-jitconfig` needs only
   the `repo` scope, so a throwaway repo plus a fine-grained PAT is enough for a real end-to-end
   loop — but webhook delivery needs a public endpoint, so the **poller is built first and
   webhooks are treated as an optimisation** validated later.

### Guardrails I want in place before touching a real cloud

Because the failure mode of a runner controller is "leaks VMs and bills you":

- a **dedicated, disposable** OVH Public Cloud project — never a shared one, so the recovery
  action is always "delete the project";
- an OVH application key restricted to `/cloud/project/<that-id>/*`;
- a low instance quota (≈10) as a hard ceiling that no bug can exceed;
- a billing alert at an agreed monthly figure;
- every e2e run ends with an assertion that the project contains zero instances, and the run
  fails if it doesn't.

### Definition of done, per milestone

No milestone counts as complete on "the code compiles". Each one lands with a test that fails if
the behaviour regresses, and the leak assertion runs in all of them.
