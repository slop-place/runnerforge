package web

import (
	"html"
	"strings"
	"testing"
)

// stripTags removes the spans, so a test can assert the visible text is
// unchanged by highlighting.
func stripTags(s string) string {
	var b strings.Builder
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteByte(s[i])
			}
		}
	}
	return b.String()
}

func TestHighlightPreservesTheText(t *testing.T) {
	t.Parallel()
	// Colouring must never change what the configuration says: an operator
	// copies this out and applies it.
	sources := map[string]string{
		formatHCL: `resource "runnerforge_pool" "ci" {
  name    = "github-large"
  enabled = true

  size_id = runnerforge_size.ovh_large.id
  labels  = ["self-hosted", "linux"]

  max_instances = 10
}

variable "ovh_password" {
  type      = string
  sensitive = true
}
`,
		formatCRD: `apiVersion: runnerforge.slop.place/v1alpha1
kind: Cloud
metadata:
  name: ovh-us-east
spec:
  driver: openstack
  enabled: true
  settings:
    auth_url: https://auth.cloud.ovh.us/v3
    region: US-EAST-VA-1
  sizes:
    - name: large
      hourlyUSD: 0.074
---
# a comment
`,
	}
	for format, src := range sources {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			out := string(highlight(format, src))
			got := html.UnescapeString(stripTags(out))
			// highlight appends a newline per line, so compare trimmed.
			if strings.TrimRight(got, "\n") != strings.TrimRight(src, "\n") {
				t.Errorf("highlighting changed the text.\n got: %q\nwant: %q", got, src)
			}
		})
	}
}

func TestHighlightEscapesHTML(t *testing.T) {
	t.Parallel()
	// A setting value is operator-supplied. Rendering it as markup would be an
	// injection into the page that shows it back.
	for _, format := range []string{formatHCL, formatCRD} {
		out := string(highlight(format, `name = "<script>alert(1)</script>"`))
		if strings.Contains(out, "<script>") {
			t.Errorf("%s: raw script tag survived: %s", format, out)
		}
		if !strings.Contains(out, "&lt;script&gt;") {
			t.Errorf("%s: the value was not escaped: %s", format, out)
		}
	}
}

func TestHighlightHCLTokens(t *testing.T) {
	t.Parallel()
	out := string(highlight(formatHCL, `resource "runnerforge_pool" "ci" {
  # a comment
  enabled       = true
  max_instances = 10
  size_id       = runnerforge_size.large.id
}`))

	for _, want := range []string{
		`<span class="t-kw">resource</span>`,
		`<span class="t-str">&#34;runnerforge_pool&#34;</span>`,
		`<span class="t-attr">enabled</span>`,
		`<span class="t-kw">true</span>`,
		`<span class="t-num">10</span>`,
		`<span class="t-ref">runnerforge_size</span>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `<span class="t-comment">  # a comment</span>`) {
		t.Errorf("the comment was not highlighted as one:\n%s", out)
	}
}

func TestHighlightYAMLTokens(t *testing.T) {
	t.Parallel()
	out := string(highlight(formatCRD, `apiVersion: runnerforge.slop.place/v1alpha1
spec:
  enabled: true
  hourlyUSD: 0.074
  quoted: "yes"
  - name: large
---`))

	for _, want := range []string{
		`<span class="t-attr">apiVersion</span>`,
		`<span class="t-kw">true</span>`,
		`<span class="t-num">0.074</span>`,
		`<span class="t-str">&#34;yes&#34;</span>`,
		`<span class="t-punct">- </span>`,
		`<span class="t-kw">---</span>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
}

func TestHighlightHandlesAwkwardInput(t *testing.T) {
	t.Parallel()
	// A URL's colon must not be read as a key separator, and an unterminated
	// string must not run away.
	out := string(highlight(formatCRD, `    auth_url: https://auth.cloud.ovh.us/v3`))
	if !strings.Contains(out, "https://auth.cloud.ovh.us/v3") {
		t.Errorf("the URL was mangled:\n%s", out)
	}

	for _, format := range []string{formatHCL, formatCRD} {
		for _, src := range []string{"", "\n\n", `name = "unterminated`, "   ", "-"} {
			// Nothing here should panic or lose content.
			got := html.UnescapeString(stripTags(string(highlight(format, src))))
			if strings.TrimRight(got, "\n") != strings.TrimRight(src, "\n") {
				t.Errorf("%s: %q became %q", format, src, got)
			}
		}
	}
}
