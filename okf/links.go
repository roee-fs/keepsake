package okf

import (
	"bytes"
	"path"
	"regexp"
	"strings"
)

// pySpace is Python's str.isspace() set, which re's \s matches for str patterns.
const pySpace = "\t\n\v\f\r \x1c\x1d\x1e\x1f\u0085\u00a0\u1680" +
	"\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a" +
	"\u2028\u2029\u202f\u205f\u3000"

func isPySpace(r rune) bool { return strings.ContainsRune(pySpace, r) }

var external = regexp.MustCompile(`(?i)^(?:[a-z][a-z0-9+.-]*://|(?:mailto|tel):|//)`)

// Resolve returns the concept path a link target names, relative to directory.
func Resolve(target, directory string) (string, bool) {
	if external.MatchString(target) {
		return "", false
	}
	target, _, _ = strings.Cut(target, "#")
	target, _, _ = strings.Cut(target, "?")
	if target == "" {
		return "", false
	}
	target = strings.TrimSuffix(target, ".md")
	if strings.HasPrefix(target, "/") {
		target, directory = strings.TrimLeft(target, "/"), ""
	}
	resolved := path.Clean(path.Join(directory, target))
	if resolved == "." || resolved == ".." || strings.HasPrefix(resolved, "../") {
		return "", false
	}
	return resolved, true
}

// dirname is posixpath.dirname: the head with trailing slashes stripped, unless it is all slashes.
func dirname(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return ""
	}
	head := p[:i+1]
	if strings.Trim(head, "/") != "" {
		head = strings.TrimRight(head, "/")
	}
	return head
}

// ExtractLinks returns the concept paths body links to, deduplicated in first-seen order.
// Inline links follow Python's (?<!!)\[[^\]]*\]\(\s*([^)\s]*)(?:\s+[^)]*)?\s*\) in linear time.
// Beyond Python, it reads reference links and <angle-bracket> destinations, and skips code.
func ExtractLinks(body, p string) []string {
	dir := dirname(p)
	body = maskCode(body)
	defs := definitions(body)
	seen := map[string]bool{}
	var out []string
	add := func(dest string) {
		if !conceptTarget(dest) {
			return
		}
		if r, ok := Resolve(dest, dir); ok && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	// closeAt is the first ']' after i, shared by every '[' before it.
	closeAt := -1
	for i := 0; i < len(body); i++ {
		if body[i] != '[' || i > 0 && body[i-1] == '!' {
			continue
		}
		if closeAt < i {
			j := strings.IndexByte(body[i:], ']')
			if j < 0 {
				break
			}
			closeAt = i + j
		}
		var next byte
		if closeAt+1 < len(body) {
			next = body[closeAt+1]
		}
		switch next {
		case '(':
			// The destination ends at the first ')'; with none, no later '[' can match either.
			open := closeAt + 2
			end := strings.IndexByte(body[open:], ')')
			if end < 0 {
				return out
			}
			add(destination(body[open : open+end]))
			i = open + end
		case '[':
			// A full [text][label] or a collapsed [text][] reference.
			end := strings.IndexByte(body[closeAt+2:], ']')
			if end < 0 {
				continue
			}
			label := body[closeAt+2 : closeAt+2+end]
			if label == "" {
				label = body[i+1 : closeAt]
			}
			// An undefined label is text, so it may still open an inline link.
			if dest, ok := defs[normalLabel(label)]; ok {
				add(dest)
				i = closeAt + 2 + end
			}
		case ':':
			// A definition, which definitions reads.
		default:
			// A shortcut [label] is a link only when the label is defined.
			if dest, ok := defs[normalLabel(body[i+1:closeAt])]; ok {
				add(dest)
				i = closeAt
			}
		}
	}
	return out
}

// destination is an inline link's target: an <angle-bracketed> path, or the first word.
func destination(inside string) string {
	dest := strings.TrimLeftFunc(inside, isPySpace)
	if strings.HasPrefix(dest, "<") {
		if k := strings.IndexByte(dest, '>'); k > 0 {
			return dest[1:k]
		}
	}
	if k := strings.IndexFunc(dest, isPySpace); k >= 0 {
		dest = dest[:k]
	}
	return dest
}

// conceptTarget reports whether dest can name a concept: not a directory, and .md or no extension.
func conceptTarget(dest string) bool {
	dest, _, _ = strings.Cut(dest, "#")
	dest, _, _ = strings.Cut(dest, "?")
	if strings.HasSuffix(dest, "/") {
		return false
	}
	ext := path.Ext(dest[strings.LastIndex(dest, "/")+1:])
	return ext == "" || strings.EqualFold(ext, ".md")
}

// A definition's line holds only the destination and an optional title, so a footnote is not one.
var definition = regexp.MustCompile(`(?m)^ {0,3}\[([^\]]+)\]:[ \t]*(<[^>\n]*>|\S+)(?:[ \t]+(?:"[^"\n]*"|'[^'\n]*'|\([^)\n]*\)))?[ \t\r]*$`)

// definitions maps each normalized reference label to its destination. The first definition wins.
func definitions(body string) map[string]string {
	defs := map[string]string{}
	if !strings.Contains(body, "]:") {
		return defs
	}
	for _, m := range definition.FindAllStringSubmatch(body, -1) {
		label := normalLabel(m[1])
		if _, ok := defs[label]; !ok {
			defs[label] = strings.TrimSuffix(strings.TrimPrefix(m[2], "<"), ">")
		}
	}
	return defs
}

// normalLabel folds case and collapses whitespace, as CommonMark matches labels.
func normalLabel(label string) string {
	return strings.ToLower(strings.Join(strings.Fields(label), " "))
}

// maskCode blanks fenced code blocks and code spans, keeping newlines, so links inside are not read.
func maskCode(body string) string {
	if !strings.ContainsAny(body, "`~") {
		return body
	}
	b := []byte(body)
	// fenceLen is the open fence's length, 0 outside a fenced block.
	var fenceChar byte
	fenceLen := 0
	for start := 0; start < len(b); {
		end := bytes.IndexByte(b[start:], '\n')
		if end < 0 {
			end = len(b)
		} else {
			end += start
		}
		trimmed := bytes.TrimLeft(b[start:end], " ")
		run := 0
		if end-start-len(trimmed) <= 3 && len(trimmed) > 0 && (trimmed[0] == '`' || trimmed[0] == '~') {
			for run < len(trimmed) && trimmed[run] == trimmed[0] {
				run++
			}
		}
		switch {
		case fenceLen > 0:
			closes := run >= fenceLen && trimmed[0] == fenceChar && len(bytes.TrimSpace(trimmed[run:])) == 0
			blank(b[start:end])
			if closes {
				fenceLen = 0
			}
		// A backtick fence's info string holds no backtick; such a line opens a code span instead.
		case run >= 3 && (trimmed[0] == '~' || bytes.IndexByte(trimmed[run:], '`') < 0):
			fenceChar, fenceLen = trimmed[0], run
			blank(b[start:end])
		}
		start = end + 1
	}
	// A code span ends with its paragraph, so each run of lines up to a blank one is masked alone.
	para := 0
	for start := 0; start <= len(b); {
		end := bytes.IndexByte(b[start:], '\n')
		if end < 0 {
			end = len(b)
		} else {
			end += start
		}
		if end == len(b) || len(bytes.TrimSpace(b[start:end])) == 0 {
			maskSpans(b[para:end])
			para = end
		}
		start = end + 1
	}
	return string(b)
}

// maskSpans blanks each code span in p: a backtick run up to the next run of the same length.
func maskSpans(p []byte) {
	type run struct{ at, n int }
	var runs []run
	for i := 0; i < len(p); {
		k := bytes.IndexByte(p[i:], '`')
		if k < 0 {
			break
		}
		at := i + k
		for i = at; i < len(p) && p[i] == '`'; i++ {
		}
		runs = append(runs, run{at, i - at})
	}
	if len(runs) < 2 {
		return
	}
	// next[r] is the next run as long as run r, or -1, which keeps pairing linear.
	next := make([]int, len(runs))
	last := map[int]int{}
	for r := len(runs) - 1; r >= 0; r-- {
		next[r] = -1
		if q, ok := last[runs[r].n]; ok {
			next[r] = q
		}
		last[runs[r].n] = r
	}
	for r := 0; r < len(runs); {
		if q := next[r]; q >= 0 {
			blank(p[runs[r].at : runs[q].at+runs[q].n])
			r = q + 1
		} else {
			r++
		}
	}
}

// blank replaces every byte of p but a newline with a space.
func blank(p []byte) {
	for k := range p {
		if p[k] != '\n' {
			p[k] = ' '
		}
	}
}
