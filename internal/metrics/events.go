package metrics

import (
	"strconv"
	"time"
)

// Everything a running controller does, counted as it happens.
//
// Label choice is deliberately conservative. Pool, cloud and forge names are
// operator-chosen and bounded by the config; job ids, machine names and error
// strings are not, and putting any of them in a label would grow the series
// count without bound. Failures are labelled by the stage that failed, which is
// the part a dashboard can act on, and the detail stays in the event log.
var (
	// The reconcile loop.
	reconcileTotal = counter("reconcile_total",
		"Reconcile passes over every enabled pool, by outcome.", "result")
	reconcileDuration = plainHistogram("reconcile_duration_seconds",
		"How long a full reconcile pass took.", loopBuckets)
	poolReconcileTotal = counter("pool_reconcile_total",
		"Reconcile passes over a single pool, by outcome.", "pool", "result")
	poolReconcileDuration = histogram("pool_reconcile_duration_seconds",
		"How long one pool took to reconcile.", loopBuckets, "pool")
	lastReconcile = plainGauge("last_reconcile_timestamp_seconds",
		"When the last reconcile pass finished, in seconds since the epoch.")

	// What the loop decided. Read these to tell a pool that is keeping up from
	// one that is pinned against its ceiling.
	jobsQueued = gauge("pool_jobs_queued",
		"Jobs waiting at the forge that this pool's labels can serve, "+
			"as of the last pass.", "pool")
	jobsUncovered = gauge("pool_jobs_uncovered",
		"Waiting jobs with no machine yet assigned to them, as of the last pass.",
		"pool")
	jobsAtCeiling = gauge("pool_jobs_at_ceiling",
		"Waiting jobs that could not be given a machine because the pool is at "+
			"max_instances. Sustained above zero means the ceiling is the "+
			"bottleneck.", "pool")
	launchesTotal = counter("launches_total",
		"Machines the controller decided to create, by why.", "pool", "kind")

	// Machines.
	machinesCreatedTotal = counter("machines_created_total",
		"Machines successfully created at a cloud.", "pool", "cloud")
	machineFailuresTotal = counter("machine_failures_total",
		"Machines that failed, by the stage that failed. Detail is in the "+
			"event log; error text would be unbounded as a label.",
		"pool", "cloud", "stage")
	machinesDestroyedTotal = counter("machines_destroyed_total",
		"Machines destroyed, by why they were destroyed.", "pool", "cloud", "reason")

	// A machine's life, in the four spans that mean something operationally.
	provisionDuration = histogram("machine_provision_seconds",
		"From the decision to launch to the provider reporting the machine "+
			"running. This is the part an operator waits on before a job can "+
			"start.", machineBuckets, "pool", "cloud")
	readyDuration = histogram("machine_ready_seconds",
		"From the decision to launch to the runner registering with the forge, "+
			"which is when the machine can actually take work.",
		machineBuckets, "pool", "cloud")
	jobDuration = histogram("machine_job_seconds",
		"How long the machine spent running its job.", machineBuckets, "pool")
	lifetimeDuration = histogram("machine_lifetime_seconds",
		"From creation to destruction, the whole life of a machine.",
		machineBuckets, "pool", "cloud")
	billedSeconds = counter("machine_billed_seconds_total",
		"Billable machine-seconds, as settled at teardown. Time spent booting "+
			"is not billable and is not counted here.", "pool", "cloud")
	spendTotal = counter("machine_cost_usd_total",
		"What destroyed machines cost, in US dollars, as settled at teardown.",
		"pool", "cloud")

	// The reaper. These are the leak detector: anything but zero means
	// something got past the happy path.
	reapTotal = counter("reap_total",
		"Reaper sweeps, by outcome.", "result")
	reapDuration = plainHistogram("reap_duration_seconds",
		"How long a reaper sweep took.", loopBuckets)
	lastReap = plainGauge("last_reap_timestamp_seconds",
		"When the last reaper sweep finished, in seconds since the epoch.")
	reapedMachinesTotal = counter("reaped_machines_total",
		"Machines the reaper destroyed, by why it had to. Any of these is a "+
			"machine the normal path failed to clean up.", "cloud", "reason")
	reapedRunnersTotal = counter("reaped_runners_total",
		"Orphaned runner registrations the reaper removed from a forge.", "forge")
	reapedRowsTotal = counter("reaped_rows_total",
		"Database rows closed out because their machine was already gone.",
		"reason")

	// Every call out to a cloud or a forge, from the one place they are all
	// built. No driver has to know it is being measured.
	cloudRequests = counter("cloud_requests_total",
		"Calls to a cloud provider's API, by operation and outcome.",
		"cloud", "driver", "operation", "result")
	cloudDuration = histogram("cloud_request_seconds",
		"How long a call to a cloud provider's API took.",
		apiBuckets, "cloud", "driver", "operation")
	forgeRequests = counter("forge_requests_total",
		"Calls to a forge's API, by operation and outcome.",
		"forge", "kind", "operation", "result")
	forgeDuration = histogram("forge_request_seconds",
		"How long a call to a forge's API took.", apiBuckets, "forge", "kind", "operation")

	// Credential checks an operator triggers from the console.
	credentialChecks = counter("credential_checks_total",
		"Credential tests run from the console or the API, by outcome.",
		"target", "name", "result")

	// The web UI and JSON API.
	httpRequests = counter("http_requests_total",
		"HTTP requests served, by route pattern and status. The route is the "+
			"pattern, not the path, so ids do not become labels.",
		"method", "route", "code")
	httpDuration = histogram("http_request_seconds",
		"How long a request took to serve.", httpBuckets, "method", "route")
	httpInFlight = plainGauge("http_requests_in_flight",
		"Requests currently being served.")

	// Sign-in and API tokens.
	authAttempts = counter("auth_attempts_total",
		"Sign-in attempts through the identity provider, by outcome.", "result")
	apiAuthTotal = counter("api_auth_total",
		"Bearer-token checks on the JSON API, by outcome.", "result")

	// The Kubernetes reconciler, when it is running.
	k8sPassTotal = counter("k8s_pass_total",
		"Passes over the cluster's custom resources, by outcome.", "result")
	k8sPassDuration = plainHistogram("k8s_pass_seconds",
		"How long a pass over the cluster's custom resources took.", loopBuckets)
	lastK8sPass = plainGauge("last_k8s_pass_timestamp_seconds",
		"When the last Kubernetes pass finished, in seconds since the epoch.")
	k8sObjects = counter("k8s_objects_total",
		"Custom resources applied to the database, by kind and outcome.",
		"kind", "result")
)

// ReconcilePass records a full pass over every pool.
func ReconcilePass(d time.Duration, err error) {
	reconcileTotal.WithLabelValues(result(err)).Inc()
	reconcileDuration.Observe(d.Seconds())
	lastReconcile.Set(float64(time.Now().Unix()))
}

// PoolReconcile records one pool's pass.
func PoolReconcile(pool string, d time.Duration, err error) {
	poolReconcileTotal.WithLabelValues(pool, result(err)).Inc()
	poolReconcileDuration.WithLabelValues(pool).Observe(d.Seconds())
}

// ScaleDecision records what one pass concluded about a pool's demand. These
// are gauges: they describe the queue as it was, not a running total.
func ScaleDecision(pool string, queued, uncovered, atCeiling int) {
	jobsQueued.WithLabelValues(pool).Set(float64(queued))
	jobsUncovered.WithLabelValues(pool).Set(float64(uncovered))
	jobsAtCeiling.WithLabelValues(pool).Set(float64(atCeiling))
}

// Launch records a decision to create a machine. Kind is "job" for a machine
// created to run a specific queued job, or "warm" for one created to hold a
// pool's min_idle.
func Launch(pool, kind string) { launchesTotal.WithLabelValues(pool, kind).Inc() }

// MachineCreated records a machine that reached the cloud.
func MachineCreated(pool, cloud string) {
	machinesCreatedTotal.WithLabelValues(pool, cloud).Inc()
}

// MachineFailed records a launch that did not get that far. Stage is where it
// stopped: "credential", "bootstrap", "provision" or "record".
func MachineFailed(pool, cloud, stage string) {
	machineFailuresTotal.WithLabelValues(pool, cloud, stage).Inc()
}

// MachineTiming records the spans of one machine's life, in seconds. A span
// that never happened is passed as a negative number and is not recorded.
func MachineTiming(pool, cloudName string, provision, ready, job, lifetime float64) {
	if provision >= 0 {
		provisionDuration.WithLabelValues(pool, cloudName).Observe(provision)
	}
	if ready >= 0 {
		readyDuration.WithLabelValues(pool, cloudName).Observe(ready)
	}
	if job >= 0 {
		jobDuration.WithLabelValues(pool).Observe(job)
	}
	if lifetime >= 0 {
		lifetimeDuration.WithLabelValues(pool, cloudName).Observe(lifetime)
	}
}

// MachineDestroyed records a teardown and what it cost.
func MachineDestroyed(pool, cloudName, reason string, billed float64, cost float64) {
	machinesDestroyedTotal.WithLabelValues(pool, cloudName, reason).Inc()
	if billed > 0 {
		billedSeconds.WithLabelValues(pool, cloudName).Add(billed)
	}
	if cost > 0 {
		spendTotal.WithLabelValues(pool, cloudName).Add(cost)
	}
}

// ReapPass records a sweep.
func ReapPass(d time.Duration, err error) {
	reapTotal.WithLabelValues(result(err)).Inc()
	reapDuration.Observe(d.Seconds())
	lastReap.Set(float64(time.Now().Unix()))
}

// ReapedMachine records a machine the reaper had to destroy.
func ReapedMachine(cloudName, reason string) {
	reapedMachinesTotal.WithLabelValues(cloudName, reason).Inc()
}

// ReapedRunner records an orphaned registration removed from a forge.
func ReapedRunner(forgeName string) { reapedRunnersTotal.WithLabelValues(forgeName).Inc() }

// ReapedRow records a database row closed out because its machine was gone.
func ReapedRow(reason string) { reapedRowsTotal.WithLabelValues(reason).Inc() }

// CloudCall records one call to a cloud provider.
func CloudCall(name, driver, op string, d time.Duration, err error) {
	cloudRequests.WithLabelValues(name, driver, op, result(err)).Inc()
	cloudDuration.WithLabelValues(name, driver, op).Observe(d.Seconds())
}

// ForgeCall records one call to a forge.
func ForgeCall(name, kind, op string, d time.Duration, err error) {
	forgeRequests.WithLabelValues(name, kind, op, result(err)).Inc()
	forgeDuration.WithLabelValues(name, kind, op).Observe(d.Seconds())
}

// CredentialCheck records a credential test. Target is "cloud" or "forge".
func CredentialCheck(target, name string, err error) {
	credentialChecks.WithLabelValues(target, name, result(err)).Inc()
}

// HTTPRequest records one served request.
func HTTPRequest(method, route string, code int, d time.Duration) {
	httpRequests.WithLabelValues(method, route, strconv.Itoa(code)).Inc()
	httpDuration.WithLabelValues(method, route).Observe(d.Seconds())
}

// HTTPInFlight moves the in-flight gauge.
func HTTPInFlight(delta float64) { httpInFlight.Add(delta) }

// AuthAttempt records a sign-in. Outcome is "success", "denied" or "error".
func AuthAttempt(outcome string) { authAttempts.WithLabelValues(outcome).Inc() }

// APIAuth records a bearer-token check on the JSON API.
func APIAuth(outcome string) { apiAuthTotal.WithLabelValues(outcome).Inc() }

// K8sPass records one pass over the cluster's custom resources.
func K8sPass(d time.Duration, err error) {
	k8sPassTotal.WithLabelValues(result(err)).Inc()
	k8sPassDuration.Observe(d.Seconds())
	lastK8sPass.Set(float64(time.Now().Unix()))
}

// K8sObject records one custom resource applied to the database.
func K8sObject(kind string, err error) {
	k8sObjects.WithLabelValues(kind, result(err)).Inc()
}
