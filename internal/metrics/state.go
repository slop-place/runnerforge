package metrics

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/slop-place/runnerforge/internal/store"
)

const (
	// scrapeTimeout bounds the queries one scrape makes. A scrape that hangs
	// on the database should fail and say so, not hold Prometheus open.
	scrapeTimeout = 5 * time.Second
	// costWindow is the window the spend gauges cover, matching the console's.
	costWindow = 24 * time.Hour
)

// StateCollector reports what is configured and what is running, read from the
// database when Prometheus scrapes.
//
// These are facts about the present, so they are collected rather than
// accumulated. After a restart the counters in this package start at zero and
// Prometheus works that out from the reset; a gauge like "machines currently
// running" has to be right on the first scrape, and the only place that knows
// is the database.
type StateCollector struct {
	db  *store.DB
	log *slog.Logger

	clouds        *prometheus.Desc
	forges        *prometheus.Desc
	pools         *prometheus.Desc
	sizes         *prometheus.Desc
	images        *prometheus.Desc
	poolCeiling   *prometheus.Desc
	poolMinIdle   *prometheus.Desc
	poolLifetime  *prometheus.Desc
	poolTimeout   *prometheus.Desc
	instances     *prometheus.Desc
	oldestMachine *prometheus.Desc
	spend         *prometheus.Desc
	spendMachines *prometheus.Desc
	spendUnpriced *prometheus.Desc
	burnRate      *prometheus.Desc
	scrapeErrors  prometheus.Counter
}

// state holds the registered collector, so that registering a second one
// replaces the first rather than panicking. There is one database behind this
// process, so there is one collector; making that idempotent is kinder than
// making the caller remember it.
var (
	stateMu      sync.Mutex
	stateCurrent *StateCollector
)

// NewStateCollector builds the collector and registers it, replacing any
// collector registered before it.
func NewStateCollector(db *store.DB, log *slog.Logger) *StateCollector {
	desc := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(namespace+"_"+name, help, labels, nil)
	}
	c := &StateCollector{
		db:  db,
		log: log,
		clouds: desc("clouds", "Configured clouds, by driver and whether they are "+
			"enabled and reachable.", "driver", "enabled", "status"),
		forges: desc("forges", "Configured forge connections, by kind and whether "+
			"they are enabled and reachable.", "kind", "enabled", "status"),
		pools:  desc("pools", "Configured pools.", "pool", "forge", "cloud", "enabled"),
		sizes:  desc("cloud_sizes", "Machine sizes in a cloud's catalogue.", "cloud"),
		images: desc("cloud_images", "Images in a cloud's catalogue.", "cloud"),
		poolCeiling: desc("pool_max_instances", "A pool's machine ceiling. Compare "+
			"with runnerforge_instances to see how much room is left.", "pool"),
		poolMinIdle: desc("pool_min_idle", "How many unbound machines a pool keeps "+
			"warm.", "pool"),
		poolLifetime: desc("pool_max_lifetime_seconds",
			"The hard ceiling on how long one of a pool's machines may live.", "pool"),
		poolTimeout: desc("pool_job_timeout_seconds",
			"How long one of a pool's machines may spend on a job.", "pool"),
		instances: desc("instances", "Machines runnerforge currently has, by state. "+
			"Compare with what the cloud reports to find leaks.",
			"pool", "cloud", "state"),
		oldestMachine: desc("oldest_machine_age_seconds",
			"Age of the oldest machine still alive in a pool. Approaching "+
				"max_lifetime means something is stuck.", "pool"),
		spend: desc("spend_usd", "What the last 24 hours cost, in US dollars, "+
			"including machines still running.", "pool"),
		spendMachines: desc("spend_machines", "Machines that billed in the last 24 "+
			"hours.", "pool"),
		spendUnpriced: desc("spend_unpriced_machines",
			"Machines in the last 24 hours whose size has no rate configured. "+
				"These contribute nothing to the spend figure, so a total of "+
				"zero here is what makes a spend of zero trustworthy.", "pool"),
		burnRate: desc("burn_rate_usd_per_hour",
			"What the machines running right now cost per hour if none of them "+
				"stopped.", "pool"),
		scrapeErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "state_scrape_errors_total",
			Help: "Scrapes where the database could not be read.",
		}),
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	if stateCurrent != nil {
		Registry.Unregister(stateCurrent)
		Registry.Unregister(stateCurrent.scrapeErrors)
	}
	Registry.MustRegister(c, c.scrapeErrors)
	stateCurrent = c
	return c
}

// Describe implements prometheus.Collector.
func (c *StateCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.clouds, c.forges, c.pools, c.sizes, c.images,
		c.poolCeiling, c.poolMinIdle, c.poolLifetime, c.poolTimeout,
		c.instances, c.oldestMachine,
		c.spend, c.spendMachines, c.spendUnpriced, c.burnRate,
	} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *StateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()

	if err := c.collectConfig(ctx, ch); err != nil {
		c.fail("read configuration", err)
		return
	}
	if err := c.collectMachines(ctx, ch); err != nil {
		c.fail("read machines", err)
	}
}

// fail records a scrape that could not read the database. It reports nothing
// rather than reporting zero: a gauge that reads zero because the database was
// unreachable would look exactly like a fleet that is idle.
func (c *StateCollector) fail(what string, err error) {
	c.scrapeErrors.Inc()
	c.log.Warn("metrics scrape could not "+what, "err", err)
}

// collectConfig reports what is configured.
func (c *StateCollector) collectConfig(ctx context.Context, ch chan<- prometheus.Metric) error {
	clouds, err := c.db.Clouds(ctx)
	if err != nil {
		return err
	}
	type cloudKey struct{ driver, enabled, status string }
	byCloud := map[cloudKey]int{}
	for i := range clouds {
		cl := &clouds[i]
		byCloud[cloudKey{cl.Driver, boolLabel(cl.Enabled), statusLabel(cl.Status)}]++
		count(ch, c.sizes, float64(len(cl.Sizes)), cl.Name)
		count(ch, c.images, float64(len(cl.Images)), cl.Name)
	}
	for k, n := range byCloud {
		count(ch, c.clouds, float64(n), k.driver, k.enabled, k.status)
	}

	forges, err := c.db.Forges(ctx)
	if err != nil {
		return err
	}
	type forgeKey struct{ kind, enabled, status string }
	byForge := map[forgeKey]int{}
	for i := range forges {
		f := &forges[i]
		byForge[forgeKey{f.Kind, boolLabel(f.Enabled), statusLabel(f.Status)}]++
	}
	for k, n := range byForge {
		count(ch, c.forges, float64(n), k.kind, k.enabled, k.status)
	}

	pools, err := c.db.Pools(ctx)
	if err != nil {
		return err
	}
	for i := range pools {
		p := &pools[i]
		count(ch, c.pools, 1, p.Name, nameOfForge(p), nameOfCloud(p), boolLabel(p.Enabled))
		count(ch, c.poolCeiling, float64(p.MaxInstances), p.Name)
		count(ch, c.poolMinIdle, float64(p.MinIdle), p.Name)
		count(ch, c.poolLifetime, p.MaxLifetime().Seconds(), p.Name)
		count(ch, c.poolTimeout, p.JobTimeout().Seconds(), p.Name)

		spend, err := c.db.PoolSpendSince(ctx, p.ID, time.Now().Add(-costWindow))
		if err != nil {
			return err
		}
		count(ch, c.spend, spend.USD, p.Name)
		count(ch, c.spendMachines, float64(spend.Machines), p.Name)
		count(ch, c.spendUnpriced, float64(spend.Unpriced), p.Name)
	}
	return nil
}

// collectMachines reports the fleet as it stands.
func (c *StateCollector) collectMachines(ctx context.Context, ch chan<- prometheus.Metric) error {
	pools, err := c.db.Pools(ctx)
	if err != nil {
		return err
	}
	// The pool list is the only place that knows which cloud a pool runs on.
	// A live instance carries its pool but not that pool's cloud, and reading
	// the label off the instance would report the same pool and state twice:
	// once under the real cloud with a zero, and once under an empty label
	// with the actual count.
	type where struct{ pool, cloud string }
	poolOf := make(map[uint]where, len(pools))

	type key struct{ pool, cloud, state string }
	counts := map[key]int{}
	oldest := map[string]time.Duration{}
	burn := map[string]float64{}

	// Every configured pool reports, including the ones with nothing running.
	// A series that disappears when a pool goes quiet cannot be alerted on.
	for i := range pools {
		p := &pools[i]
		w := where{p.Name, nameOfCloud(p)}
		poolOf[p.ID] = w
		oldest[p.Name] = 0
		burn[p.Name] = 0
		for _, st := range store.InstanceStates() {
			counts[key{w.pool, w.cloud, string(st)}] = 0
		}
	}

	live, err := c.db.AllLiveInstances(ctx)
	if err != nil {
		return err
	}
	for i := range live {
		in := &live[i]
		w, ok := poolOf[in.PoolID]
		if !ok {
			// A machine whose pool was deleted underneath it. It is still
			// running and still costing money, so it is reported rather than
			// dropped; the reaper is what makes it go away.
			w = where{pool: "", cloud: ""}
		}
		counts[key{w.pool, w.cloud, string(in.State)}]++
		if age := time.Since(in.CreatedAt); age > oldest[w.pool] {
			oldest[w.pool] = age
		}
		burn[w.pool] += in.HourlyUSD
	}

	for k, n := range counts {
		count(ch, c.instances, float64(n), k.pool, k.cloud, k.state)
	}
	for pool, age := range oldest {
		count(ch, c.oldestMachine, age.Seconds(), pool)
	}
	for pool, rate := range burn {
		count(ch, c.burnRate, rate, pool)
	}
	return nil
}

// count emits one gauge sample.
func count(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
}

// boolLabel renders a bool as a label value.
func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// statusLabel renders a credential-check result, giving the never-checked case
// a name of its own rather than an empty label.
func statusLabel(s string) string {
	if s == "" {
		return "unchecked"
	}
	return s
}

func nameOfCloud(p *store.Pool) string {
	if p.Cloud != nil {
		return p.Cloud.Name
	}
	return ""
}

func nameOfForge(p *store.Pool) string {
	if p.Forge != nil {
		return p.Forge.Name
	}
	return ""
}
