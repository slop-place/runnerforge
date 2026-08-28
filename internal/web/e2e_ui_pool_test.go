package web_test

import (
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// seedCloudAndForge configures a cloud with one size and one image, and a
// forge, through the same forms a person would use. Tests that are about
// something later in the journey start from here.
func (u *ui) seedCloudAndForge() {
	u.t.Helper()
	u.addCloud("cloud-a")
	id := u.cloudIDNamed("cloud-a")
	u.submit("add a size", "/clouds/"+id, `form[action$="/sizes"]`, map[string]string{
		"name": "small", "f_flavor": "c3-4", "hourly_usd": "0.0740",
	}, "c3-4")
	u.submit("add an image", "/clouds/"+id, `form[action$="/images"]`, map[string]string{
		"name": "ci-base", "f_image_id": "img-9",
	}, "ci-base")
	u.submit("add the forge", "/forges?kind=uifake", `form.form`, map[string]string{
		"name": "forge-a", "f_test_id": u.t.Name(),
	}, "forge-a")
}

// addCloud configures one cloud of the browser-test driver.
func (u *ui) addCloud(name string) {
	u.t.Helper()
	u.submit("add "+name, "/clouds?driver=uifake", `form.form`, map[string]string{
		"name": name, "f_test_id": u.t.Name(),
	}, name)
}

// addPool configures a pool over the seeded cloud and forge, taking jobs with
// the given label.
func (u *ui) addPool(labels string) {
	u.t.Helper()
	u.submit("add pool-a", "/pools", `form.form`, map[string]string{
		"name": "pool-a", "labels": labels, "max_instances": "2",
	}, "pool-a")
}

// TestUIPoolFormLoadsCloudCatalogue checks the part of this UI that cannot
// work without htmx: choosing a cloud repopulates the size and image pickers
// from that cloud's catalogue, because sizes belong to one account.
func TestUIPoolFormLoadsCloudCatalogue(t *testing.T) {
	u := newUI(t)
	u.seedCloudAndForge()

	// A second cloud with a catalogue of its own, so the swap has something
	// to actually change.
	u.addCloud("cloud-b")
	u.submit("give it a size of its own", "/clouds/"+u.cloudIDNamed("cloud-b"),
		`form[action$="/sizes"]`, map[string]string{
			"name": "enormous", "f_flavor": "c3-64",
		}, "enormous")

	u.goTo("/pools")
	// The form opens on the first cloud, so it offers that cloud's sizes.
	u.waitIn(`#shape`, "small")
	if got := u.text(`#shape`); strings.Contains(got, "enormous") {
		t.Errorf("the size picker is offering another cloud's sizes: %q", got)
	}

	// Switching clouds swaps the pickers without leaving the page. If this
	// stopped working a pool could be saved with a size that does not exist
	// on its cloud.
	u.run("switch cloud", chromedp.SetValue(
		`select[name="cloud_id"]`, u.cloudIDNamed("cloud-b"), chromedp.ByQuery))
	u.run("fire the change", chromedp.Evaluate(
		`document.querySelector('select[name="cloud_id"]').dispatchEvent(`+
			`new Event('change', {bubbles: true}))`, nil))
	u.waitIn(`#shape`, "enormous")
	if got := u.text(`#shape`); strings.Contains(got, "small") {
		t.Errorf("the size picker kept the old cloud's sizes: %q", got)
	}
	u.assertQuiet()
}

// cloudIDNamed returns the option value for a cloud, so a test can pick one
// by the name it gave it rather than by a database id it has to guess.
func (u *ui) cloudIDNamed(name string) string {
	u.t.Helper()
	clouds, err := u.db.Clouds(u.t.Context())
	if err != nil {
		u.t.Fatal(err)
	}
	for _, c := range clouds {
		if c.Name == name {
			return itoa(c.ID)
		}
	}
	u.t.Fatalf("no cloud named %q", name)
	return ""
}

// TestUIPoolToRunningMachine is the whole point of the console: a pool is
// created through the form, a job turns up on the forge, and the dashboard
// shows the machine that was created for it — without the page being
// reloaded, because the tables refresh themselves.
func TestUIPoolToRunningMachine(t *testing.T) {
	u := newUI(t)
	u.seedCloudAndForge()

	u.addPool("browser-test")

	// Land on the dashboard first, then queue the job, so that what the test
	// observes is the page updating on its own rather than a fresh render.
	u.goTo("/")
	u.waitText("pool-a")
	if n := u.count(`.state`); n != 0 {
		t.Fatalf("%d machines exist before any job was queued", n)
	}

	u.forge.enqueue("browser-test")

	// The controller notices the job and provisions for it.
	u.waitFor("the cloud to hold a machine", func() bool { return u.cloud.live() == 1 })

	// The page the browser is already sitting on picks the machine up on its
	// next refresh. Nothing reloaded it; if the polling broke, this is where
	// it shows.
	u.waitFor("the dashboard to show the machine on its own", func() bool {
		return u.count(`.state`) == 1
	})
	if body := u.bodyText(); !strings.Contains(body, "pool-a") {
		t.Error("the machine is not attributed to its pool on the dashboard")
	}

	// The machine has a page of its own that names what it belongs to.
	machines, err := u.db.AllLiveInstances(u.t.Context())
	if err != nil || len(machines) != 1 {
		t.Fatalf("live machines: %v, %v", machines, err)
	}
	u.goTo("/instances/" + itoa(machines[0].ID))
	page := u.bodyText()
	for _, want := range []string{machines[0].Name, "pool-a"} {
		if !strings.Contains(page, want) {
			t.Errorf("the machine page does not mention %q", want)
		}
	}
	// The state is whatever the machine has reached by now, but the page must
	// be showing one.
	if n := u.count(`.state`); n != 1 {
		t.Errorf("the machine page shows %d states, want 1", n)
	}

	// Destroying from the console reaches the cloud, not only the row.
	u.goTo("/instances")
	u.waitFor("the machine table", func() bool { return u.count(`.state`) == 1 })
	destroy := `form[hx-post="/instances/` + itoa(machines[0].ID) + `/destroy"] button`
	u.waitFor("the machine's destroy button", func() bool { return u.count(destroy) == 1 })
	u.run("destroy it", chromedp.Click(destroy, chromedp.ByQuery))
	u.waitFor("the cloud to be empty", func() bool { return u.cloud.live() == 0 })

	// The console keeps the machine listed but shows it as gone, which is what
	// an operator wants after a teardown: the row, its lifetime and its cost.
	u.goTo("/")
	u.waitFor("the dashboard to show the machine as deleted", func() bool {
		return u.count(`.state.deleted`) == 1
	})
	if !strings.Contains(u.bodyText(), machines[0].Name) {
		t.Error("the destroyed machine vanished from the console entirely")
	}
	u.assertQuiet()
}
