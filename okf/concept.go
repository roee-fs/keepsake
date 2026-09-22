// Package okf is the Open Knowledge Format: concept parsing, link extraction and validation.
// It MUST be publishable without the rest of keepsake.
package okf

// Map is a stub; Task 3 replaces it with the frontmatter value domain.
type Map struct{}

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
