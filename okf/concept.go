// Package okf is the Open Knowledge Format: concept parsing, link extraction and validation.
// It MUST be publishable without the rest of keepsake.
package okf

// Concept is a single knowledge unit: a path, its frontmatter and its markdown body.
type Concept struct {
	Path        string
	Type        string
	Title       string
	Description string
	Body        string
	Frontmatter *Map
	Links       []string
	Version     int
}

// Parse reads an OKF document. Frontmatter keeps only the fields Concept does not promote.
func Parse(text, path string) (Concept, error) {
	fm, body, err := split(text)
	if err != nil {
		return Concept{}, err
	}
	return Concept{
		Path:        path,
		Type:        promote(fm, "type"),
		Title:       promote(fm, "title"),
		Description: promote(fm, "description"),
		Body:        body,
		Frontmatter: fm,
		Links:       ExtractLinks(body, path),
		Version:     1,
	}, nil
}

// promote pops a known field. A key with no value is blank, not the word None.
func promote(fm *Map, key string) string {
	v, _ := fm.Get(key)
	fm.Delete(key)
	if v == nil {
		return ""
	}
	return pyStr(v)
}
