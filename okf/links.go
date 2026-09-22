package okf

import (
	"path"
	"regexp"
	"strings"
)

// pySpace is Python's str.isspace() set, which re's \s matches for str patterns.
const pySpace = `[\t\n\v\f\r \x1c-\x1f\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

// isPySpace reports whether r is in the same set as pySpace, for use with strings.TrimFunc.
func isPySpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return (r >= 0x1c && r <= 0x1f) || (r >= 0x2000 && r <= 0x200a)
}

var (
	link     = regexp.MustCompile(`^\[[^\]]*\]\(` + pySpace + `*([^)\t\n\v\f\r \x1c-\x1f\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]*)(?:` + pySpace + `+[^)]*)?` + pySpace + `*\)`)
	external = regexp.MustCompile(`(?i)^(?:[a-z][a-z0-9+.-]*://|(?:mailto|tel):|//)`)
)

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
func ExtractLinks(body, p string) []string {
	dir := dirname(p)
	seen := map[string]bool{}
	var out []string
	for i := 0; i < len(body); {
		if body[i] == '[' && (i == 0 || body[i-1] != '!') {
			if m := link.FindStringSubmatchIndex(body[i:]); m != nil {
				if r, ok := Resolve(body[i+m[2]:i+m[3]], dir); ok && !seen[r] {
					seen[r] = true
					out = append(out, r)
				}
				i += max(m[1], 1)
				continue
			}
		}
		i++
	}
	return out
}
