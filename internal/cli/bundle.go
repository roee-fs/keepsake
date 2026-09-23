// A bundle of markdown files is how knowledge enters and leaves; Postgres is the only
// store. The index and log a bundle carries are generated at export time and are never
// stored as concepts. Ported from 2de90d2:src/keepsake/cli/__init__.py.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/internal/store"
	"github.com/roee-fs/keepsake/okf"
)

const actor = "cli"

// The log is a bundle file an operator reads, not an audit export.
const logLimit = 1000

const topLevel = "(top level)"

// documents returns every concept in the bundle, the generated files excluded. It
// parses the whole bundle before returning, so a malformed file refuses the command
// rather than aborting it half-applied. strict=false returns invalid concepts instead,
// for validate to report them all at once.
func documents(root string, strict bool) ([]okf.Concept, error) {
	// WalkDir does not follow a symlinked root, and rglob does. Files are still named
	// by the root the operator typed.
	resolved, err := filepath.EvalSymlinks(root)
	if errors.Is(err, fs.ErrNotExist) {
		// pathlib's rglob finds nothing in a directory that does not exist.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rels []string
	err = filepath.WalkDir(resolved, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".md") {
			rel, err := filepath.Rel(resolved, p)
			rels = append(rels, filepath.ToSlash(rel))
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// pathlib orders paths by their parts, not by their full string.
	slices.SortFunc(rels, func(a, b string) int {
		return slices.Compare(strings.Split(a, "/"), strings.Split(b, "/"))
	})

	var concepts []okf.Concept
	for _, rel := range rels {
		path := strings.TrimSuffix(rel, ".md")
		if okf.ReservedPaths[path] {
			continue
		}
		file := filepath.Join(root, rel)
		text, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		concept, err := okf.Parse(string(text), path)
		if err != nil {
			// The parser names the frontmatter it was handed, never the file it came from.
			return nil, fmt.Errorf("%s: %v", file, err)
		}
		// Python lets json.dumps write NaN, which Postgres then refuses mid-import.
		if nonFinite(concept.Frontmatter) {
			return nil, fmt.Errorf("%s: frontmatter holds NaN or Infinity, which JSON cannot store", file)
		}
		// Storing an invalid concept is worse than refusing it: a later okf_update
		// merges the stored empty type back in and fails on a field nobody touched.
		if errs := okf.Validate(concept); strict && len(errs) > 0 {
			return nil, fmt.Errorf("%s: %s", file, strings.Join(errs, "; "))
		}
		concepts = append(concepts, concept)
	}
	return concepts, nil
}

func nonFinite(v any) bool {
	switch v := v.(type) {
	case okf.NonFinite:
		return true
	case []any:
		return slices.ContainsFunc(v, nonFinite)
	case *okf.Map:
		for _, k := range v.Keys() {
			if x, _ := v.Get(k); nonFinite(x) {
				return true
			}
		}
	}
	return false
}

// ImportBundle stores every concept in the bundle in one transaction, so the
// all-or-nothing documents promises across parsing holds across writing too.
func ImportBundle(ctx context.Context, cs *store.ConceptStore, tenant uuid.UUID, root string) (int, error) {
	concepts, err := documents(root, true)
	if err != nil {
		return 0, err
	}
	n, err := cs.ImportMany(ctx, tenant, concepts, actor)
	if errors.Is(err, store.ErrNotFound) {
		// Python renders the KeyError's path; ErrNotFound wraps it as "not found: <path>".
		path := strings.TrimPrefix(err.Error(), store.ErrNotFound.Error()+": ")
		return 0, fmt.Errorf("%s was removed while the bundle was importing", path)
	}
	return n, err
}

// target is the file a concept is written to, relative to the bundle root. It refuses
// rather than leave the bundle.
func target(path string) (string, error) {
	if okf.ReservedPaths[path] {
		return "", fmt.Errorf("the concept %s collides with a generated file: the bundle root reserves index.md and log.md",
			okf.PyReprString(path))
	}
	// A traversing path cannot be written through a tool, but whatever is stored, the
	// export MUST NOT write outside the directory the operator named.
	name := path + ".md"
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("refusing to write %s outside the bundle", okf.PyReprString(path))
	}
	return name, nil
}

// ExportBundle writes the corpus out as a bundle, index and log included.
func ExportBundle(ctx context.Context, cs *store.ConceptStore, tenant uuid.UUID, root string) (int, error) {
	if err := os.MkdirAll(root, 0o777); err != nil {
		return 0, err
	}
	// Written through os.Root, so a symlink inside the bundle cannot lead a write out of it.
	dir, err := os.OpenRoot(root)
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	concepts, err := cs.ReadAll(ctx, tenant)
	if err != nil {
		return 0, err
	}
	// Every path is checked before the first file is written: a bundle that is half
	// written looks like a complete one.
	targets := make([]string, len(concepts))
	paths := make([]string, len(concepts))
	for i, c := range concepts {
		if targets[i], err = target(c.Path); err != nil {
			return 0, err
		}
		paths[i] = c.Path
	}
	for i, c := range concepts {
		text, err := okf.Serialize(c)
		if err != nil {
			return 0, err
		}
		if err := dir.MkdirAll(filepath.Dir(targets[i]), 0o777); err != nil {
			return 0, err
		}
		if err := dir.WriteFile(targets[i], []byte(text), 0o666); err != nil {
			return 0, err
		}
	}
	if err := dir.WriteFile("index.md", []byte(renderIndex(paths)), 0o666); err != nil {
		return 0, err
	}
	log, err := renderLog(ctx, cs, tenant, logLimit)
	if err != nil {
		return 0, err
	}
	if err := dir.WriteFile("log.md", []byte(log), 0o666); err != nil {
		return 0, err
	}
	return len(paths), nil
}

// ValidateBundle returns every rule the bundle breaks: per-concept errors plus links to nothing.
func ValidateBundle(root string) ([]string, error) {
	concepts, err := documents(root, false)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, c := range concepts {
		known[c.Path] = true
	}
	var errs []string
	for _, c := range concepts {
		for _, e := range okf.Validate(c) {
			errs = append(errs, c.Path+": "+e)
		}
		for _, l := range c.Links {
			if !known[l] {
				errs = append(errs, c.Path+": link to unknown concept "+l)
			}
		}
	}
	return errs, nil
}

// renderIndex groups the corpus by its first path segment.
func renderIndex(paths []string) string {
	groups := map[string][]string{}
	for _, p := range paths {
		first, _, found := strings.Cut(p, "/")
		if !found {
			first = topLevel
		}
		groups[first] = append(groups[first], p)
	}
	lines := []string{"# Index", ""}
	for _, g := range slices.Sorted(maps.Keys(groups)) {
		lines = append(lines, "## "+g, "")
		for _, p := range groups[g] {
			lines = append(lines, fmt.Sprintf("- [%s](%s.md)", p, p))
		}
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// renderLog renders the most recent limit revisions, oldest first, under date headings.
func renderLog(ctx context.Context, cs *store.ConceptStore, tenant uuid.UUID, limit int) (string, error) {
	revisions, err := cs.Revisions(ctx, tenant, limit)
	if err != nil {
		return "", err
	}
	lines := []string{"# Log", ""}
	day := ""
	for _, r := range revisions {
		stamp := r.Day
		if stamp != day {
			day = stamp
			lines = append(lines, "## "+stamp, "")
		}
		lines = append(lines, fmt.Sprintf("- `%s` v%d %s by %s", r.Path, r.Version, r.Op, r.UpdatedBy))
	}
	if len(revisions) == limit {
		lines = append(lines, "", fmt.Sprintf("Older revisions omitted: the log renders at most %d.", limit))
	}
	lines = append(lines, "")
	return strings.Join(lines, "\n"), nil
}
