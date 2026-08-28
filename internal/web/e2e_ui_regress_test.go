package web_test

import (
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/slop-place/runnerforge/internal/store"
)

// TestUILiveTablesWorkOnFirstPaint is a regression test for a console that
// looked right and did nothing.
//
// The live tables used to arrive empty and fill themselves in from
// /partials/*, triggered by htmx on load. htmx did not wire up the controls
// inside that first response, so the destroy button on the dashboard and the
// machines page silently reloaded the page instead of destroying anything —
// until the next poll replaced the table, after which it worked. The tables
// are server-rendered now and htmx only refreshes them.
func TestUILiveTablesWorkOnFirstPaint(t *testing.T) {
	u := newUI(t)
	u.seedCloudAndForge()
	u.addPool("first-paint")
	u.forge.enqueue("first-paint")
	u.waitFor("a machine", func() bool { return u.cloud.live() == 1 })

	// A freshly loaded page, with no poll having happened yet: the table is
	// in the very first response.
	u.goTo("/instances")
	if n := u.count(`.state`); n != 1 {
		t.Fatalf("the machine table is not in the page as served: %d rows", n)
	}
	if n := u.count(`.muted`); n > 0 && strings.Contains(u.bodyText(), "loading") {
		t.Error("the page still ships a loading placeholder")
	}

	// htmx must have wired the controls in that first response. Clicking one
	// has to reach the cloud.
	u.run("destroy from the first paint",
		chromedp.Click(`form[hx-post$="/destroy"] button`, chromedp.ByQuery))
	u.waitFor("the destroy button to reach the cloud",
		func() bool { return u.cloud.live() == 0 })
	u.assertQuiet()
}

// TestUIDeleteJourney removes everything through the console, in an order
// that is wrong first and then right, and checks the console both refuses and
// agrees.
func TestUIDeleteJourney(t *testing.T) {
	u := newUI(t)
	u.seedCloudAndForge()

	u.addPool("doomed")

	cloudID := u.cloudIDNamed("cloud-a")

	// A cloud cannot go while a pool still points at it. The console has to
	// refuse and say why, rather than leaving a pool aimed at nothing.
	u.goTo("/clouds/" + cloudID)
	u.run("try to delete the cloud",
		chromedp.Click(`a[href$="/delete"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`main h1`, chromedp.ByQuery))
	clouds, err := u.db.Clouds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(clouds) == 0 {
		t.Fatal("deleting a cloud took a pool's cloud out from under it")
	}
	// The refusal is a flash of its own, not the standing warning about the
	// console being unprotected.
	if got := strings.ToLower(u.text(`.flash.bad + .flash, .flash:not(.bad)`)); !strings.Contains(got, "pool") {
		t.Errorf("the console did not explain the refusal; it said %q", got)
	}

	// The catalogue entries come off one row at a time, without a reload.
	u.goTo("/clouds/" + cloudID)
	before := u.count(`table tbody tr`)
	u.run("remove the size",
		chromedp.Click(`form[hx-post^="/sizes/"] button`, chromedp.ByQuery))
	u.waitFor("the size row to go", func() bool { return u.count(`table tbody tr`) == before-1 })

	// In the right order it all goes.
	pools, err := u.db.Pools(t.Context())
	if err != nil || len(pools) != 1 {
		t.Fatalf("pools: %v, %v", pools, err)
	}
	u.goTo("/pools/" + itoa(pools[0].ID))
	u.run("delete the pool", chromedp.Click(`a[href$="/delete"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`main h1`, chromedp.ByQuery))
	u.waitText("No pools")

	u.goTo("/clouds/" + cloudID)
	u.run("delete the cloud", chromedp.Click(`a[href$="/delete"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`main h1`, chromedp.ByQuery))
	u.waitText("No clouds configured yet")

	forges, err := u.db.Forges(t.Context())
	if err != nil || len(forges) != 1 {
		t.Fatalf("forges: %v, %v", forges, err)
	}
	u.goTo("/forges/" + itoa(forges[0].ID))
	u.run("delete the forge", chromedp.Click(`a[href$="/delete"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`main h1`, chromedp.ByQuery))
	u.waitText("No forges configured yet")
	u.assertQuiet()
}

// TestUIReapButton checks the sweep in the header actually sweeps: a machine
// the database has forgotten is still the controller's to clean up.
func TestUIReapButton(t *testing.T) {
	u := newUI(t)
	u.seedCloudAndForge()
	u.addPool("reapable")
	u.forge.enqueue("reapable")
	u.waitFor("a machine", func() bool { return u.cloud.live() == 1 })

	// Lose the row, the way a restore from an old backup would. The machine
	// is still running and still costing money.
	machines, err := u.db.AllLiveInstances(t.Context())
	if err != nil || len(machines) != 1 {
		t.Fatalf("live machines: %v, %v", machines, err)
	}
	if err := u.db.WithContext(t.Context()).Unscoped().
		Delete(&store.Instance{}, machines[0].ID).Error; err != nil {
		t.Fatal(err)
	}

	u.goTo("/")
	u.run("reap", chromedp.Click(`form[hx-post="/reap"] button`, chromedp.ByQuery))
	u.waitFor("the reaper to find the orphan", func() bool { return u.cloud.live() == 0 })
	u.assertQuiet()
}

// TestMetricsEndpointIsReachableWithoutSigningIn checks the scrape endpoint
// sits outside the sign-in gate.
//
// Prometheus cannot complete an authorization code flow, so a /metrics behind
// OIDC is a /metrics nothing can read — and the failure is silent: the console
// works, the endpoint answers, and every scrape quietly collects a redirect to
// a login page.
func TestMetricsEndpointIsReachableWithoutSigningIn(t *testing.T) {
	u := newUI(t)
	u.seedCloudAndForge()
	u.addPool("scraped")
	u.forge.enqueue("scraped")
	u.waitFor("a machine", func() bool { return u.cloud.live() == 1 })

	body, code := u.get("/metrics")
	if code != 200 {
		t.Fatalf("GET /metrics returned %d", code)
	}
	for _, want := range []string{
		"# HELP runnerforge_instances",
		"# TYPE runnerforge_instances gauge",
		`runnerforge_instances{cloud="cloud-a",pool="pool-a",state="booting"}`,
		`runnerforge_machines_created_total{cloud="cloud-a",pool="pool-a"}`,
		`runnerforge_pool_max_instances{pool="pool-a"} 2`,
		// The endpoint is served through the same middleware as everything
		// else, so a scrape is itself a timed request.
		"runnerforge_http_requests_total",
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scrape does not contain %q", want)
		}
	}
	// A stored credential must never reach a page an operator will paste into
	// a ticket.
	if strings.Contains(body, "sk-") {
		t.Error("the scrape looks like it contains a credential")
	}
	u.assertQuiet()
}
