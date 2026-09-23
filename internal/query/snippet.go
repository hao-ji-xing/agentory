package query

import (
	"strings"
	"unicode"
)

// Span is a half-open rune range [Start, End) inside a string.
type Span struct{ Start, End int }

// FindAll returns the non-overlapping, case-insensitive occurrences of any
// term in text, as rune offsets sorted by position.
func FindAll(text []rune, terms []Term) []Span {
	lower := make([]rune, len(text))
	for i, r := range text {
		lower[i] = unicode.ToLower(r)
	}
	var spans []Span
	for _, t := range terms {
		needle := []rune(strings.ToLower(t.Text))
		if len(needle) == 0 {
			continue
		}
		for i := 0; i+len(needle) <= len(lower); {
			if equalRunes(lower[i:i+len(needle)], needle) {
				spans = append(spans, Span{i, i + len(needle)})
				i += len(needle)
			} else {
				i++
			}
		}
	}
	// Sort and merge overlaps between different terms.
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j].Start < spans[j-1].Start; j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
	var out []Span
	for _, s := range spans {
		if n := len(out); n > 0 && s.Start <= out[n-1].End {
			if s.End > out[n-1].End {
				out[n-1].End = s.End
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

func equalRunes(a, b []rune) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Flatten collapses runs of whitespace into single spaces.
func Flatten(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Snippet returns a window of at most width runes of the flattened text,
// positioned so that the first match is visible, with "…" marking cuts.
func Snippet(text string, terms []Term, width int) string {
	r := []rune(Flatten(text))
	if width <= 0 || len(r) <= width {
		return string(r)
	}
	start := 0
	if spans := FindAll(r, terms); len(spans) > 0 {
		start = spans[0].Start - width/4
	}
	if start < 0 {
		start = 0
	}
	if start+width > len(r) {
		start = len(r) - width
	}
	out := string(r[start : start+width])
	if start > 0 {
		out = "…" + out
	}
	if start+width < len(r) {
		out += "…"
	}
	return out
}

// Highlight wraps every match of terms in text with pre/post.
func Highlight(text string, terms []Term, pre, post string) string {
	r := []rune(text)
	spans := FindAll(r, terms)
	if len(spans) == 0 {
		return text
	}
	var sb strings.Builder
	last := 0
	for _, s := range spans {
		sb.WriteString(string(r[last:s.Start]))
		sb.WriteString(pre)
		sb.WriteString(string(r[s.Start:s.End]))
		sb.WriteString(post)
		last = s.End
	}
	sb.WriteString(string(r[last:]))
	return sb.String()
}
