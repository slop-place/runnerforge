// Package export renders runnerforge's configuration as Terraform HCL and as
// Kubernetes custom resources.
//
// The point is that the UI is not a dead end. Someone clicking through forms to
// get a pool working can then take the same thing away as code, rather than
// rebuilding it by hand and hoping the two agree. Both renderers read the same
// records and the same driver-declared field schemas the forms are built from,
// so what the UI shows is what the API stores.
//
// Secrets are never rendered. HCL emits a variable reference and CRDs emit a
// secretRef, because a configuration you can paste into a repository must not
// contain the credential.
package export

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/store"
)

// APIVersion is the group and version CRDs are published under.
const APIVersion = "runnerforge.slop.place/v1alpha1"

// hclIdent turns a display name into a Terraform identifier.
//
// Terraform labels cannot contain most punctuation, so "ovh-us-east" is fine
// but "OVH US East" is not.
func hclIdent(name string) string {
	var b strings.Builder
	prev := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prev = false
		default:
			if !prev && b.Len() > 0 {
				b.WriteByte('_')
				prev = true
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "unnamed"
	}
	// An identifier cannot start with a digit.
	if out[0] >= '0' && out[0] <= '9' {
		out = "r" + out
	}
	return out
}

// k8sName turns a display name into a DNS-1123 subdomain, which is what a
// Kubernetes object name has to be.
func k8sName(name string) string {
	var b strings.Builder
	prev := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prev = false
		default:
			if !prev && b.Len() > 0 {
				b.WriteByte('-')
				prev = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unnamed"
	}
	return out
}

// hclString quotes a value for HCL.
func hclString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`, "${", `$${`)
	return `"` + r.Replace(s) + `"`
}

// hclValue renders a settings value in its natural HCL type, so a number stays
// a number rather than becoming a quoted string the provider has to parse back.
func hclValue(v any) string {
	switch t := v.(type) {
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case string:
		return hclString(t)
	default:
		return hclString(fmt.Sprint(t))
	}
}

// yamlValue renders a settings value for YAML.
func yamlValue(v any) string {
	switch t := v.(type) {
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case string:
		// Quote anything that YAML would otherwise reinterpret.
		if t == "" || needsYAMLQuote(t) {
			return strconv.Quote(t)
		}
		return t
	default:
		return strconv.Quote(fmt.Sprint(t))
	}
}

// needsYAMLQuote reports whether a scalar would be misread unquoted.
func needsYAMLQuote(s string) bool {
	if strings.ContainsAny(s, ":#{}[]&*!|>'\"%@`,\n") {
		return true
	}
	if strings.TrimSpace(s) != s {
		return true
	}
	switch strings.ToLower(s) {
	case "true", "false", "null", "yes", "no", "on", "off", "~":
		return true
	}
	// A bare number would become a number.
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	return false
}

// sortedKeys returns a map's keys in a stable order, so the same configuration
// always renders identically and a diff means something changed.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// secretFields returns the keys a driver or forge declared as secret, so the
// renderers know what to replace with a reference rather than a value.
func secretFields(fields []cloud.Field) map[string]cloud.Field {
	out := map[string]cloud.Field{}
	for _, f := range fields {
		if f.Secret {
			out[f.Key] = f
		}
	}
	return out
}

// labelsList renders a StringList for HCL.
func labelsList(l store.StringList) string {
	parts := make([]string, 0, len(l))
	for _, s := range l {
		parts = append(parts, hclString(s))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
