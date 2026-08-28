package web_test

import (
	"strings"
	"testing"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

// TestUIExportButtons follows the buttons that turn a record made in the forms
// into config you can commit: the page must offer both formats, highlight what
// it shows, and never print the credential.
func TestUIExportButtons(t *testing.T) {
	u := newUI(t)
	u.submit("add a cloud with a secret", "/clouds?driver=uifake", `form.form`,
		map[string]string{
			"name": "cloud-a", "f_test_id": t.Name(),
			"f_region": "us-east-1", "f_api_key": "sk-do-not-print-me",
		}, "cloud-a")

	u.run("as terraform",
		chromedp.Click(`a[href^="/export/cloud/"][href$="/hcl"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`pre.code`, chromedp.ByQuery))
	hcl := u.text(`pre.code`)
	for _, want := range []string{"resource", "runnerforge_cloud", "cloud-a", "uifake"} {
		if !strings.Contains(hcl, want) {
			t.Errorf("the Terraform export is missing %q:\n%s", want, hcl)
		}
	}
	assertNoSecret(t, u.bodyText(), "the Terraform export")
	// Highlighting is server-rendered, so the page must arrive with the
	// tokens already marked up rather than colouring itself afterwards.
	if n := u.count(`pre.code .t-kw`); n == 0 {
		t.Error("nothing in the Terraform export is highlighted as a keyword")
	}
	if n := u.count(`pre.code .t-str`); n == 0 {
		t.Error("no strings are highlighted in the Terraform export")
	}

	u.run("as kubernetes",
		chromedp.Click(`a[href^="/export/cloud/"][href$="/crd"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`pre.code`, chromedp.ByQuery))
	crd := u.text(`pre.code`)
	for _, want := range []string{"apiVersion", "kind: Cloud", "cloud-a", "secretRef"} {
		if !strings.Contains(crd, want) {
			t.Errorf("the Kubernetes export is missing %q:\n%s", want, crd)
		}
	}
	assertNoSecret(t, u.bodyText(), "the Kubernetes export")
	if n := u.count(`pre.code .t-attr`); n == 0 {
		t.Error("no keys are highlighted in the Kubernetes export")
	}

	// The whole configuration is reachable from the dashboard in both formats.
	u.goTo("/")
	u.run("whole config as terraform",
		chromedp.Click(`a[href="/export/all/hcl"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`pre.code`, chromedp.ByQuery))
	if got := u.text(`pre.code`); !strings.Contains(got, "cloud-a") {
		t.Errorf("the whole-config export does not include the cloud:\n%s", got)
	}
	assertNoSecret(t, u.bodyText(), "the whole-config export")
	u.assertQuiet()
}

// assertNoSecret fails if a stored credential turns up in something meant to
// be committed to a repository.
func assertNoSecret(t *testing.T, page, what string) {
	t.Helper()
	if strings.Contains(page, "sk-do-not-print-me") {
		t.Errorf("%s printed the stored secret", what)
	}
}

// TestUIRespectsColourScheme checks the console follows the browser's theme
// rather than picking one, in both directions.
func TestUIRespectsColourScheme(t *testing.T) {
	u := newUI(t)

	background := func() string {
		var bg string
		u.run("read the background", chromedp.Evaluate(
			`getComputedStyle(document.body).backgroundColor`, &bg))
		return bg
	}

	u.run("prefer light", emulation.SetEmulatedMedia().
		WithFeatures([]*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: "light"}}))
	u.goTo("/")
	light := background()

	u.run("prefer dark", emulation.SetEmulatedMedia().
		WithFeatures([]*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: "dark"}}))
	u.goTo("/")
	dark := background()

	if light == dark {
		t.Errorf("the console renders %s in both light and dark", light)
	}
	if !isDarker(dark, light) {
		t.Errorf("the dark theme (%s) is not darker than the light one (%s)", dark, light)
	}
	u.assertQuiet()
}

// isDarker compares two rgb() colours by total brightness.
func isDarker(a, b string) bool { return brightness(a) < brightness(b) }

// brightness sums the channels of an rgb() or rgba() string.
func brightness(c string) int {
	c = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(c, "rgba("), "rgb("), ")")
	total := 0
	for i, part := range strings.Split(c, ",") {
		if i > 2 {
			break
		}
		n := 0
		for _, ch := range strings.TrimSpace(part) {
			if ch < '0' || ch > '9' {
				break
			}
			n = n*10 + int(ch-'0')
		}
		total += n
	}
	return total
}
