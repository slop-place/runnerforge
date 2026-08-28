package web

import (
	"html"
	"html/template"
	"strings"
)

// Syntax highlighting for the rendered configuration.
//
// Done here rather than in the browser: the console ships no JavaScript beyond
// htmx, and a highlighter is a large dependency to take on for four colours.
// These are line-based tokenizers, which is enough for the two shapes
// runnerforge emits and cannot mangle input it does not understand — anything
// unrecognised comes out as plain, escaped text.

// token wraps text in a class, escaping it.
func token(class, text string) string {
	if text == "" {
		return ""
	}
	if class == "" {
		return html.EscapeString(text)
	}
	return `<span class="t-` + class + `">` + html.EscapeString(text) + `</span>`
}

// highlight dispatches on the export format.
func highlight(format, src string) template.HTML {
	var b strings.Builder
	for line := range strings.SplitSeq(src, "\n") {
		if format == formatCRD {
			b.WriteString(highlightYAMLLine(line))
		} else {
			b.WriteString(highlightHCLLine(line))
		}
		b.WriteByte('\n')
	}
	// Every fragment written above is either escaped text or a span this
	// function produced, so the result is safe to render as HTML.
	return template.HTML(b.String()) //nolint:gosec // G203: all input is escaped by token()
}

// hclKeywords are the words that open a block or carry meaning on their own.
var hclKeywords = map[string]bool{
	"resource": true, "variable": true, "provider": true, "terraform": true,
	"required_providers": true, "module": true, "output": true, "data": true,
	"locals": true, "true": true, "false": true, "null": true,
}

// highlightHCLLine tokenizes one line of HCL.
func highlightHCLLine(line string) string {
	// A comment runs to the end of the line and contains nothing else.
	if trimmed := strings.TrimLeft(line, " \t"); strings.HasPrefix(trimmed, "#") ||
		strings.HasPrefix(trimmed, "//") {
		return token("comment", line)
	}

	var b strings.Builder
	rest := line

	// Leading indentation is kept verbatim so the block structure survives.
	indent := len(rest) - len(strings.TrimLeft(rest, " \t"))
	b.WriteString(html.EscapeString(rest[:indent]))
	rest = rest[indent:]

	for rest != "" {
		var emitted string
		switch {
		case rest[0] == '"':
			emitted = scanHCLString(rest)
			b.WriteString(token("str", emitted))
		case isIdentStart(rest[0]):
			emitted = scanIdent(rest)
			b.WriteString(token(hclIdentClass(emitted, rest[len(emitted):]), emitted))
		case rest[0] >= '0' && rest[0] <= '9':
			emitted = scanNumber(rest)
			b.WriteString(token("num", emitted))
		default:
			emitted = rest[:1]
			b.WriteString(token("", emitted))
		}
		rest = rest[len(emitted):]
	}
	return b.String()
}

// scanHCLString returns the string literal at the head of s, to the next
// unescaped quote or the end of the line.
func scanHCLString(s string) string {
	end := 1
	for end < len(s) {
		if s[end] == '\\' {
			end += 2
			continue
		}
		if s[end] == '"' {
			end++
			break
		}
		end++
	}
	if end > len(s) {
		return s
	}
	return s[:end]
}

// scanIdent returns the identifier at the head of s.
func scanIdent(s string) string {
	end := 0
	for end < len(s) && isIdentPart(s[end]) {
		end++
	}
	return s[:end]
}

// scanNumber returns the number at the head of s.
func scanNumber(s string) string {
	end := 0
	for end < len(s) && (s[end] == '.' || (s[end] >= '0' && s[end] <= '9')) {
		end++
	}
	return s[:end]
}

// hclIdentClass decides what an identifier is, from the word and what follows.
func hclIdentClass(word, rest string) string {
	switch {
	case hclKeywords[word]:
		return "kw"
	case strings.HasPrefix(rest, "."):
		// A reference such as runnerforge_cloud.name.id.
		return "ref"
	case attributeFollows(rest):
		return "attr"
	default:
		return ""
	}
}

// attributeFollows reports whether what comes next makes the preceding word an
// attribute name rather than a value.
func attributeFollows(rest string) bool {
	trimmed := strings.TrimLeft(rest, " \t")
	return strings.HasPrefix(trimmed, "=")
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '-'
}

// highlightYAMLLine tokenizes one line of YAML.
func highlightYAMLLine(line string) string {
	trimmed := strings.TrimLeft(line, " ")
	indent := line[:len(line)-len(trimmed)]

	switch {
	case trimmed == "":
		return html.EscapeString(line)
	case strings.HasPrefix(trimmed, "#"):
		return token("comment", line)
	case strings.HasPrefix(trimmed, "---"):
		return token("kw", line)
	}

	var b strings.Builder
	b.WriteString(html.EscapeString(indent))

	// A list item keeps its dash, then is tokenized like any other line.
	if strings.HasPrefix(trimmed, "- ") {
		b.WriteString(token("punct", "- "))
		trimmed = trimmed[2:]
	} else if trimmed == "-" {
		return b.String() + token("punct", "-")
	}

	// A key is everything up to the first colon that is followed by a space or
	// ends the line; anything else is a bare value.
	if key, value, ok := splitYAMLKey(trimmed); ok {
		b.WriteString(token("attr", key))
		b.WriteString(token("punct", ":"))
		b.WriteString(highlightYAMLValue(value))
		return b.String()
	}
	b.WriteString(highlightYAMLValue(trimmed))
	return b.String()
}

// splitYAMLKey separates a key from its value.
func splitYAMLKey(s string) (string, string, bool) {
	for i := range len(s) {
		if s[i] != ':' {
			continue
		}
		if i+1 == len(s) {
			return s[:i], "", true
		}
		if s[i+1] == ' ' {
			return s[:i], s[i+1:], true
		}
		// A colon inside a value, such as a URL.
		return "", "", false
	}
	return "", "", false
}

// highlightYAMLValue tokenizes the value half of a line.
func highlightYAMLValue(v string) string {
	trimmed := strings.TrimLeft(v, " ")
	lead := v[:len(v)-len(trimmed)]
	if trimmed == "" {
		return html.EscapeString(v)
	}

	// A trailing comment is separated out so the value is not coloured as one.
	var comment string
	if i := strings.Index(trimmed, " #"); i >= 0 {
		comment = trimmed[i:]
		trimmed = trimmed[:i]
	}

	class := ""
	switch {
	case strings.HasPrefix(trimmed, `"`), strings.HasPrefix(trimmed, "'"):
		class = "str"
	case trimmed == "true", trimmed == "false", trimmed == "null":
		class = "kw"
	case isNumeric(trimmed):
		class = "num"
	}
	return html.EscapeString(lead) + token(class, trimmed) + token("comment", comment)
}

// isNumeric reports whether a scalar is a plain number.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	dots := 0
	for i, c := range []byte(s) {
		switch {
		case c >= '0' && c <= '9':
		case c == '.':
			dots++
			if dots > 1 {
				return false
			}
		case c == '-' && i == 0:
		default:
			return false
		}
	}
	return true
}
