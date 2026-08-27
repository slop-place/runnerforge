package web

import (
	"maps"
	"net/http"
	"strconv"
	"strings"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/store"
)

// renderField is one form input, with its current value resolved.
//
// Drivers declare what they need; this is the shape the templates render it in.
type renderField struct {
	cloud.Field

	// Value is what to pre-fill. Never populated for secrets — a stored secret
	// is shown as "set", not as its value.
	Value string
	// IsSet reports that a secret already has a value, so the form can say so
	// without revealing it.
	IsSet bool
	// Name is the form input name, namespaced to avoid colliding with the
	// record's own fields.
	Name string
	// Opts are the resolved choices for a select, with the stored one marked.
	// Resolving selection here rather than in the template keeps the comparison
	// out of a place that cannot express it cleanly.
	Opts []renderOption
}

// renderOption is one choice in a select, with its selected state resolved.
type renderOption struct {
	Value    string
	Label    string
	Detail   string
	Selected bool
}

// fieldPrefix namespaces driver-declared inputs so a driver cannot define a
// field called "name" and overwrite the record's own name.
const fieldPrefix = "f_"

// buildFields resolves a schema against stored values for rendering.
func buildFields(fields []cloud.Field, settings store.Params, creds store.Secret) []renderField {
	out := make([]renderField, 0, len(fields))
	for _, f := range fields {
		rf := renderField{Field: f, Name: fieldPrefix + f.Key}
		switch {
		case f.Secret:
			// Secrets are never sent back to the browser. The form only says
			// whether one is stored, so an operator editing a region does not
			// have to re-enter a password they cannot see.
			rf.IsSet = creds[f.Key] != ""
		case f.Type == cloud.FieldBool:
			if b, ok := settings[f.Key].(bool); ok && b {
				rf.Value = "on"
			}
		default:
			rf.Value = paramString(settings, f.Key)
			if rf.Value == "" {
				rf.Value = f.Default
			}
		}
		for _, o := range f.Options {
			rf.Opts = append(rf.Opts, renderOption{
				Value: o.Value, Label: o.Label, Selected: o.Value == rf.Value,
			})
		}
		out = append(out, rf)
	}
	return out
}

// withCatalog replaces a field's options with a live catalogue listing, marking
// the stored value as selected. Used for the flavor and image pickers, where
// the alternative is an operator typing a UUID from another browser tab.
func withCatalog(fields []renderField, key string, items []cloud.CatalogItem) []renderField {
	for i := range fields {
		if fields[i].Key != key {
			continue
		}
		opts := make([]renderOption, 0, len(items)+1)
		// A stored value the catalogue no longer offers must still be visible,
		// or saving the form would silently change it.
		known := false
		for _, it := range items {
			sel := it.ID == fields[i].Value
			known = known || sel
			opts = append(opts, renderOption{
				Value: it.ID, Label: it.Label, Detail: it.Detail, Selected: sel,
			})
		}
		if fields[i].Value != "" && !known {
			opts = append([]renderOption{{
				Value: fields[i].Value, Label: fields[i].Value,
				Detail: "no longer offered", Selected: true,
			}}, opts...)
		}
		fields[i].Opts = opts
	}
	return fields
}

// paramString renders a stored param as form input text, tolerating the number
// types JSON round-trips through.
func paramString(p store.Params, key string) string {
	switch v := p[key].(type) {
	case string:
		return v
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

// collectFields reads a submitted form back into settings and credentials.
//
// Existing secrets are preserved when their input is left blank, so saving a
// form does not silently erase a credential the operator could not see.
func collectFields(r *http.Request, fields []cloud.Field, existing store.Secret) (store.Params, store.Secret, error) {
	settings := store.Params{}
	creds := store.Secret{}
	maps.Copy(creds, existing)

	var missing []string
	for _, f := range fields {
		raw := strings.TrimSpace(r.FormValue(fieldPrefix + f.Key))

		if f.Secret {
			if raw != "" {
				creds[f.Key] = raw
			}
			if f.Required && creds[f.Key] == "" {
				missing = append(missing, f.Label)
			}
			continue
		}

		switch f.Type {
		case cloud.FieldBool:
			// An unchecked box submits nothing at all, which is how it is
			// distinguished from a checked one.
			settings[f.Key] = raw != ""
		case cloud.FieldNumber:
			if raw == "" {
				break
			}
			n, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return nil, nil, &fieldError{Label: f.Label, Reason: "must be a number"}
			}
			settings[f.Key] = n
		case cloud.FieldText, cloud.FieldPassword, cloud.FieldSelect:
			if raw != "" {
				settings[f.Key] = raw
			}
		}

		if f.Required && !f.Secret && raw == "" && f.Type != cloud.FieldBool {
			missing = append(missing, f.Label)
		}
	}

	if len(missing) > 0 {
		return nil, nil, &fieldError{Label: strings.Join(missing, ", "), Reason: "is required"}
	}
	return settings, creds, nil
}

// fieldError names the field an operator has to go and fix.
type fieldError struct {
	Label  string
	Reason string
}

func (e *fieldError) Error() string { return e.Label + " " + e.Reason }

// errNameRequired is the one field every record needs and no driver declares.
var errNameRequired = &fieldError{Label: "Name", Reason: "is required"}
