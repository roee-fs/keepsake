package okf

import (
	"path"
	"regexp"
	"strings"
)

// pySpace is Python's str.isspace() set, which re's \s matches for str patterns.
const pySpace = "\t\n\v\f\r \x1c\x1d\x1e\x1f\u0085\u00a0\u1680" +
	"\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a" +
	"\u2028\u2029\u202f\u205f\u3000"

func isPySpace(r rune) bool { return strings.ContainsRune(pySpace, r) }

var (
	link     = regexp.MustCompile(`^\[[^\]]*\]\([` + pySpace + `]*([^)` + pySpace + `]*)(?:[` + pySpace + `]+[^)]*)?[` + pySpace + `]*\)`)
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
