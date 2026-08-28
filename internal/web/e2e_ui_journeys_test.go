package web_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestUINavigation walks the header links and checks each page arrives with
// the content it promises and the current link marked.
func TestUINavigation(t *testing.T) {
	u := newUI(t)
	pages := []struct{ link, path, heading string }{
		{"Dashboard", "/", "Dashboard"},
		{"Pools", "/pools", "Pools"},
		{"Clouds", "/clouds", "Clouds"},
		{"Forges", "/forges", "Forges"},
		{"Machines", "/instances", "Machines"},
		{"Events", "/events", "Events"},
	}
	u.goTo("/")
	for _, p := range pages {
		u.run("click "+p.link,
			chromedp.Click(fmt.Sprintf(`header nav a[href=%q]`, p.path), chromedp.ByQuery),
			chromedp.WaitVisible(`main h1`, chromedp.ByQuery),
		)
		if got := u.text(`main h1`); !strings.Contains(got, p.heading) {
			t.Errorf("%s: heading is %q, want it to contain %q", p.link, got, p.heading)
		}
		// The nav marks where you are, which is the only orientation this
		// console offers.
		if n := u.count(fmt.Sprintf(`header nav a[href=%q].on`, p.path)); n != 1 {
			t.Errorf("%s: %d nav links marked current, want 1", p.link, n)
		}
	}
	u.assertQuiet()
}

// TestUIWarnsWhenUnprotected checks the console says so when no OIDC issuer is
// configured. Losing this warning would be the kind of regression nobody
// notices until the console is on the internet.
func TestUIWarnsWhenUnprotected(t *testing.T) {
	u := newUI(t)
	u.goTo("/")
	if body := u.bodyText(); !strings.Contains(body, "not protected") {
		t.Error("an unauthenticated console did not warn that it is unprotected")
	}
	if n := u.count(`.flash.bad`); n == 0 {
		t.Error("the warning is not rendered as a warning")
	}
	u.assertQuiet()
}

// TestUICreateCloudJourney adds a cloud, a size and an image entirely through
// the forms, the way an operator would, and then checks the credential test
// button swaps the row in place rather than reloading the page.
func TestUICreateCloudJourney(t *testing.T) {
	u := newUI(t)

	u.goTo("/clouds")
	if body := u.bodyText(); !strings.Contains(body, "No clouds configured yet") {
		t.Error("a fresh install did not say it has no clouds")
	}

	// Step one is picking a driver; the form that follows is that driver's.
	u.run("pick the driver",
		chromedp.Click(`.picker a[href="/clouds?driver=uifake"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`form.form input[name="f_test_id"]`, chromedp.ByQuery),
	)
	// The form asks for what this driver needs and nothing else.
	if n := u.count(`form.form input[name="f_flavor"]`); n != 0 {
		t.Error("the connection form is showing size fields")
	}
	// A secret field must never be a plain text input.
	if n := u.count(`form.form input[name="f_api_key"][type="password"]`); n != 1 {
		t.Error("the API key field is not a password field")
	}

	// Adding a cloud lands on its own page, which is where its catalogue of
	// sizes and images lives.
	u.run("add the cloud",
		u.fill(`input[name="name"]`, "browser-cloud"),
		u.fill(`input[name="f_test_id"]`, t.Name()),
		u.fill(`input[name="f_region"]`, "us-east-1"),
		u.fill(`input[name="f_api_key"]`, "sk-secret-value"),
		chromedp.Click(`form.form .actions button`, chromedp.ByQuery),
		chromedp.WaitVisible(`form[action$="/sizes"]`, chromedp.ByQuery),
	)
	id := u.cloudIDNamed("browser-cloud")
	if got := u.text(`main h1`); !strings.Contains(got, "browser-cloud") {
		t.Errorf("after adding, the page is %q, want the new cloud's", got)
	}
	// The secret is stored, and the console never shows it back.
	if body := u.bodyText(); strings.Contains(body, "sk-secret-value") {
		t.Error("the console rendered a stored secret back to the page")
	}
	if n := u.count(`input[name="f_api_key"][value="sk-secret-value"]`); n != 0 {
		t.Error("the stored secret is sitting in a form value")
	}

	u.submit("add a size", "/clouds/"+id, `form[action$="/sizes"]`, map[string]string{
		"name": "small", "f_flavor": "c3-4", "vcpus": "4",
		"memory_mb": "8192", "hourly_usd": "0.0740",
	}, "c3-4")
	u.submit("add an image", "/clouds/"+id, `form[action$="/images"]`, map[string]string{
		"name": "ci-base", "f_image_id": "img-9", "username": "debian",
	}, "ci-base")

	// Back on the list, testing the credential is an HTMX post that replaces
	// one row in place. If the swap did not happen the row still says the
	// cloud was never checked, and the size and image counts would be lost.
	u.goTo("/clouds")
	if got := u.text(`table tbody tr`); !strings.Contains(got, "unchecked") {
		t.Errorf("a cloud that was never tested reads %q", got)
	}
	u.run("test the credential",
		chromedp.Click(`table tbody tr form[hx-post$="/check"] button`, chromedp.ByQuery))
	u.waitIn(`table tbody tr`, "reachable")
	if n := u.count(`table tbody tr`); n != 1 {
		t.Errorf("after the check there are %d cloud rows, want 1", n)
	}
	// The swap replaced the row, not the page: the name, driver and the size
	// and image counts all survived it.
	row := strings.ToLower(u.text(`table tbody tr`))
	for _, want := range []string{"browser-cloud", "uifake", "1"} {
		if !strings.Contains(row, want) {
			t.Errorf("the swapped row lost %q; it reads %q", want, row)
		}
	}
	u.assertQuiet()
}

// TestUIRequiredFieldsBlockSubmit checks the browser refuses an incomplete
// form, so a half-configured cloud never reaches the database.
func TestUIRequiredFieldsBlockSubmit(t *testing.T) {
	u := newUI(t)
	u.goTo("/clouds?driver=uifake")
	u.run("submit an empty form",
		chromedp.Click(`form.form .actions button`, chromedp.ByQuery),
		chromedp.WaitVisible(`form.form input[name="name"]:invalid`, chromedp.ByQuery),
	)
	clouds, err := u.db.Clouds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(clouds) != 0 {
		t.Errorf("an invalid form still wrote %d clouds", len(clouds))
	}
	u.assertQuiet()
}
