// Package models holds the values the repositories return.
package models

import (
	"time"

	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/okf"
)

// Revision is one entry in the revision log, without the snapshot that holds the body.
type Revision struct {
	Path      string
	Version   int
	Op        string
	UpdatedBy string
	CreatedAt time.Time
	TenantID  uuid.UUID
	// Day is CreatedAt's date in the session TimeZone, as psycopg dates it.
	Day string
}

// Hit is one search result. It carries no body, only the passages that match: the agent
// searches, chooses, then reads.
type Hit struct {
	Path        string
	Type        string
	Title       string
	Description string
	Score       float64
	Frontmatter *okf.Map
	Snippet     string
}

// Summary is a concept table row: Hit without the score or frontmatter, plus TenantID for mixed-tenant pages.
type Summary struct {
	Path        string
	Type        string
	Title       string
	Description string
	Version     int
	UpdatedAt   time.Time
	TenantID    uuid.UUID
}

// Totals are corpus-wide counts for the admin console's summary tiles.
type Totals struct {
	Concepts  int
	ByType    map[string]int
	Revisions int
	Links     int
	Orphans   int
}

// PathType is one (path, type) pair, as List returns.
type PathType struct {
	Path string
	Type string
}

// DailyWrite is one day's concept-revision count.
type DailyWrite struct {
	Date  time.Time
	Count int
}

// TenantCount is one tenant and how many concepts it holds.
type TenantCount struct {
	TenantID uuid.UUID
	Count    int
}

// GrepHit is one (path, snippet) match; the snippet is a card, not a body.
type GrepHit struct {
	Path    string
	Snippet string
}

// GraphRow is (path, type, title, links) for one concept, as Graph returns.
type GraphRow struct {
	Path  string
	Type  string
	Title string
	Links []string
}

// Conflict is a stale expected version. CurrentBody is the text to merge against.
type Conflict struct {
	CurrentVersion int
	CurrentBody    string
}
