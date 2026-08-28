package web_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/slop-place/runnerforge/internal/config"
	"github.com/slop-place/runnerforge/internal/controller"
	"github.com/slop-place/runnerforge/internal/store"
	"github.com/slop-place/runnerforge/internal/web"
)

// These drive the console in a real browser against the real server, because
// the handler tests cannot see the half of this UI that only exists once a
// browser runs it: HTMX swapping a row in place, a select that repopulates
// another select, a table that refreshes on a timer, and the highlighting that
// turns exported config into something readable.
//
// Nothing here is stubbed except the cloud and the forge, which are in-process
// fakes; every byte between the browser and the database is production code.
//
// Skipped when no Chrome is installed. Set RF_TEST_UI_HEADED=1 to watch.

const (
	// uiTimeout bounds one whole browser test. Chrome start-up dominates it.
	uiTimeout = 90 * time.Second
	// uiAction bounds a single browser step, so a selector that stopped
	// matching fails with the page in hand instead of exhausting uiTimeout.
	uiAction = 15 * time.Second
	// uiPoll is how long an assertion waits for the UI to catch up with the
	// controller. Refresh intervals in the templates are 3s and 5s.
	uiPoll = 25 * time.Second
)

// chromePath finds an installed Chrome, or "" when there is none.
func chromePath() string {
	if p := os.Getenv("RF_TEST_CHROME"); p != "" {
		return p
	}
	candidates := []string{"google-chrome", "chromium", "chromium-browser"}
	if goruntime.GOOS == "darwin" {
		candidates = append([]string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}, candidates...)
	}
	for _, c := range candidates {
		if strings.Contains(c, "/") {
			if _, err := os.Stat(c); err == nil {
				return c
			}
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

// ui is one browser pointed at one freshly built runnerforge.
type ui struct {
	t     *testing.T
	ctx   context.Context //nolint:containedctx // the browser session this ui drives
	base  string
	db    *store.DB
	ctrl  *controller.Controller
	cloud *uiCloud
	forge *uiForge

	mu   sync.Mutex
	errs []string // console errors and failed requests seen by the browser
}

// newUI boots a server, a controller loop and a browser, and tears all three
// down when the test ends.
func newUI(t *testing.T) *ui {
	t.Helper()

	chrome := chromePath()
	if chrome == "" {
		t.Skip("no Chrome found; set RF_TEST_CHROME to run the browser tests")
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := store.SetKey(key); err != nil {
		t.Fatal(err)
	}
	secret, err := config.GenerateSecretKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ID: "rf-ui-test", SecretKey: secret,
		Database: config.Database{Driver: "sqlite", DSN: t.TempDir() + "/ui.db"},
	}
	db, err := store.Open(cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	ctrl := controller.New(db, cfg, log)

	srv := httptest.NewServer(web.New(db, ctrl, cfg, log, nil).Handler())
	t.Cleanup(srv.Close)
	cfg.BaseURL = srv.URL

	u := &ui{
		t: t, base: srv.URL, db: db, ctrl: ctrl,
		cloud: newUICloud(t.Name()), forge: newUIForge(t.Name()),
	}

	// A reconcile loop, so a queued job turns into a machine while the browser
	// is looking at the page.
	loopCtx, stopLoop := context.WithCancel(context.Background())
	var loopDone sync.WaitGroup
	loopDone.Go(func() {
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-tick.C:
				_ = ctrl.ReconcileAll(loopCtx)
			}
		}
	})
	t.Cleanup(func() {
		stopLoop()
		loopDone.Wait()
	})

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chrome),
		chromedp.Flag("headless", os.Getenv("RF_TEST_UI_HEADED") == ""),
		chromedp.WindowSize(1440, 1000),
	)
	alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	t.Cleanup(cancelAlloc)
	browser, cancelBrowser := chromedp.NewContext(alloc)
	t.Cleanup(cancelBrowser)
	timed, cancelTimeout := context.WithTimeout(browser, uiTimeout)
	t.Cleanup(cancelTimeout)
	u.ctx = timed

	// Start the browser on the test-wide context. chromedp launches on the
	// first Run, and every Run after this one is on a short-lived child
	// context whose cancellation would otherwise take the browser with it.
	if err := chromedp.Run(u.ctx); err != nil {
		t.Fatalf("start chrome: %v", err)
	}

	u.watchConsole()
	return u
}

// run executes browser actions and fails the test on error. A failure prints
// the page, because a selector that stopped matching is otherwise a timeout
// with nothing to go on.
func (u *ui) run(what string, actions ...chromedp.Action) {
	u.t.Helper()
	ctx, cancel := context.WithTimeout(u.ctx, uiAction)
	defer cancel()
	if err := chromedp.Run(ctx, actions...); err != nil {
		u.t.Fatalf("%s: %v\npage was:\n%s", what, err, u.dump())
	}
}

// dump returns the current page's HTML, best effort.
func (u *ui) dump() string {
	var html string
	ctx, cancel := context.WithTimeout(u.ctx, 5*time.Second)
	defer cancel()
	var where string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`location.pathname + location.search`, &where),
		chromedp.OuterHTML(`body`, &html, chromedp.ByQuery),
	); err != nil {
		return "(could not read the page: " + err.Error() + ")"
	}
	return where + "\n" + html
}

// goTo loads a page and waits for the shell to be present.
func (u *ui) goTo(path string) {
	u.t.Helper()
	u.run("navigate to "+path,
		chromedp.Navigate(u.base+path),
		chromedp.WaitVisible(`header.bar .brand`, chromedp.ByQuery),
	)
}

// text returns the visible text of the first match, or "" if nothing matches.
//
// Reading through the DOM API rather than a node handle matters here: htmx
// replaces elements while these tests are looking at them, and a handle taken
// a moment earlier refers to a node that no longer exists.
func (u *ui) text(sel string) string {
	u.t.Helper()
	var s string
	u.run("read "+sel, chromedp.Evaluate(
		fmt.Sprintf(`(document.querySelector(%q)?.innerText ?? "")`, sel), &s))
	return s
}

// bodyText returns everything the user can read on the page.
func (u *ui) bodyText() string {
	u.t.Helper()
	return u.text("body")
}

// fill puts a value in a field, replacing whatever is there, and fires the
// events a real keystroke would so that anything listening reacts.
func (u *ui) fill(sel, value string) chromedp.Action {
	return chromedp.Tasks{
		chromedp.WaitVisible(sel, chromedp.ByQuery),
		chromedp.Evaluate(fmt.Sprintf(`(() => {
		  const el = document.querySelector(%q);
		  el.value = %q;
		  el.dispatchEvent(new Event("input", {bubbles: true}));
		  el.dispatchEvent(new Event("change", {bubbles: true})) })()`, sel, value), nil),
	}
}

// submit fills a form and sends it, then waits for the page it lands on.
//
// Every write in this console redirects, so filling and submitting have to
// happen on a document that has finished arriving. Starting from a fresh
// navigation is what makes that true: a value written into a document the
// browser is already replacing is lost, the form posts an empty required
// field, and the browser blocks a submit the test then waits forever for.
func (u *ui) submit(what, page, form string, fields map[string]string, landed string) {
	u.t.Helper()
	u.goTo(page)
	actions := make([]chromedp.Action, 0, len(fields)+3)
	actions = append(actions, chromedp.WaitVisible(form, chromedp.ByQuery))
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		actions = append(actions, u.fill(form+` [name="`+name+`"]`, fields[name]))
	}
	actions = append(actions,
		chromedp.Click(form+` .actions button`, chromedp.ByQuery),
		chromedp.WaitVisible(`main h1`, chromedp.ByQuery))
	u.run(what, actions...)
	if landed != "" {
		u.waitText(landed)
	}
}

// get fetches a path from the server directly, without the browser. Used for
// endpoints a browser is not the client for.
func (u *ui) get(path string) (string, int) {
	u.t.Helper()
	req, err := http.NewRequestWithContext(u.t.Context(), http.MethodGet, u.base+path, nil)
	if err != nil {
		u.t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		u.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		u.t.Fatal(err)
	}
	return string(body), resp.StatusCode
}

// count returns how many elements match, which is how these tests assert that
// a row appeared or a row went away.
func (u *ui) count(sel string) int {
	u.t.Helper()
	var n int
	u.run("count "+sel, chromedp.Evaluate(
		fmt.Sprintf(`document.querySelectorAll(%q).length`, sel), &n))
	return n
}

// waitFor polls until cond holds, so a test can wait on the page catching up
// with the controller without guessing how long that takes.
func (u *ui) waitFor(what string, cond func() bool) {
	u.t.Helper()
	deadline := time.Now().Add(uiPoll)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		select {
		case <-u.ctx.Done():
			u.t.Fatalf("waiting for %s: browser context ended: %v", what, u.ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	u.t.Fatalf("timed out after %s waiting for %s\npage was:\n%s", uiPoll, what, u.dump())
}

// waitIn polls until the text of sel contains want, and reports what it last
// saw when it does not.
func (u *ui) waitIn(sel, want string) {
	u.t.Helper()
	var last string
	deadline := time.Now().Add(uiPoll)
	for time.Now().Before(deadline) {
		last = u.text(sel)
		if strings.Contains(last, want) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	u.t.Fatalf("%s never contained %q; it says %q\npage was:\n%s", sel, want, last, u.dump())
}

// waitText polls until the page contains want.
func (u *ui) waitText(want string) {
	u.t.Helper()
	u.waitFor(fmt.Sprintf("%q on the page", want), func() bool {
		return strings.Contains(u.bodyText(), want)
	})
}

// itoa renders an id for a form value.
func itoa(id uint) string { return strconv.FormatUint(uint64(id), 10) }

// watchConsole records console errors and failed requests. A page that renders
// correctly but throws on every HTMX swap is broken, and only the browser can
// say so; every test ends by asserting this stayed empty.
func (u *ui) watchConsole() {
	chromedp.ListenTarget(u.ctx, func(ev any) {
		switch e := ev.(type) {
		case *runtime.EventConsoleAPICalled:
			if e.Type != "error" {
				return
			}
			parts := make([]string, 0, len(e.Args))
			for _, a := range e.Args {
				parts = append(parts, string(a.Value))
			}
			u.note("console: " + strings.Join(parts, " "))
		case *runtime.EventExceptionThrown:
			u.note("exception: " + e.ExceptionDetails.Text)
		case *network.EventLoadingFailed:
			u.note("request failed: " + e.ErrorText)
		}
	})
	u.run("enable events", network.Enable())
}

// note records a browser-side problem.
func (u *ui) note(s string) {
	// Fonts come from Google and the tests run offline; that is not a defect
	// in this console.
	if strings.Contains(s, "fonts.g") {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.errs = append(u.errs, s)
}

// assertQuiet fails if the browser reported anything while the test ran.
func (u *ui) assertQuiet() {
	u.t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.errs) > 0 {
		u.t.Errorf("the browser reported %d problems:\n  %s",
			len(u.errs), strings.Join(u.errs, "\n  "))
	}
}
