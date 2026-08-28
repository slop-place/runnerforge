// Package metrics publishes what runnerforge is doing in Prometheus's text
// format.
//
// The metrics are grouped by the question they answer, because a runner
// controller is watched for a few specific reasons:
//
//   - Is it keeping up? Jobs waiting, machines launching, how long a machine
//     takes to become useful.
//   - Is it leaking? Machines the cloud reports against machines runnerforge
//     thinks it has, and what the reaper had to clean up.
//   - What is it costing? Spend and billed time, per pool and per cloud.
//   - Is anything broken? Every call to a cloud or a forge, timed and counted
//     by outcome.
//
// Counters live in this package and are updated as things happen. Anything that
// is a fact about the present — how many machines exist, what a pool's ceiling
// is, what the last day cost — is read from the database when Prometheus
// scrapes, by the collector in state.go. That split matters after a restart: a
// counter starts at zero and Prometheus handles the reset, but a gauge read
// from the database is correct immediately.
package metrics

import (
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// namespace prefixes every metric this process publishes.
const namespace = "runnerforge"

// Registry holds everything runnerforge publishes.
//
// This is a registry of our own rather than the client library's default one:
// the default is package-global state that any dependency can write to, and a
// test that wants to read a counter would be reading whatever else the process
// had registered.
var Registry = prometheus.NewRegistry()

// Buckets are chosen from what these operations actually take, since a bucket
// set that does not straddle the real distribution answers no question at all.
var (
	// apiBuckets cover a call to a cloud or forge API: tens of milliseconds
	// when it is healthy, tens of seconds when it is not.
	apiBuckets = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	// httpBuckets cover serving a page, which is a database read and a
	// template.
	httpBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5}
	// loopBuckets cover a reconcile or reap pass, which is bounded by how many
	// clouds and forges it has to talk to.
	loopBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}
	// machineBuckets cover a machine's life, from a boot that takes half a
	// minute to a job that runs for an hour.
	machineBuckets = []float64{5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 14400}
)

func init() {
	Registry.MustRegister(
		// What the Go runtime and the process itself are doing. A controller
		// that leaks goroutines or file descriptors is a controller that will
		// stop provisioning at three in the morning.
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewBuildInfoCollector(),
	)
}

// SetBuildInfo publishes the deployment's identity as a labelled constant, the
// conventional way to make a version joinable onto any other series.
func SetBuildInfo(version, controllerID string) {
	buildInfo.WithLabelValues(version, runtime.Version(), controllerID).Set(1)
}

// buildInfo is always 1; the labels are the payload.
var buildInfo = gauge("build_info",
	"Always 1. The labels carry the build and the deployment identity, so a "+
		"dashboard can join a version onto anything else.",
	"version", "go_version", "controller_id")

// counter registers a counter vector under the namespace.
func counter(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: name, Help: help,
	}, labels)
	Registry.MustRegister(c)
	return c
}

// gauge registers a gauge vector under the namespace.
func gauge(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: name, Help: help,
	}, labels)
	Registry.MustRegister(g)
	return g
}

// plainGauge registers a gauge that carries no labels.
func plainGauge(name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Name: name, Help: help,
	})
	Registry.MustRegister(g)
	return g
}

// plainHistogram registers a histogram that carries no labels.
func plainHistogram(name, help string, buckets []float64) prometheus.Histogram {
	h := prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Name: name, Help: help, Buckets: buckets,
	})
	Registry.MustRegister(h)
	return h
}

// histogram registers a histogram vector under the namespace.
func histogram(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Name: name, Help: help, Buckets: buckets,
	}, labels)
	Registry.MustRegister(h)
	return h
}

// result turns an error into a label value, so that every "did it work" label
// in this package has the same two values and a dashboard can sum across them.
func result(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
}
