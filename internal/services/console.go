package services

import (
	"context"

	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/internal/models"
	"github.com/roee-fs/keepsake/internal/repositories"
	"github.com/roee-fs/keepsake/okf"
)

const (
	// Past a few hundred nodes a force layout is unreadable anyway.
	graphLimit = 500
	// Separate from the node cap: one concept can link to thousands of targets.
	graphEdgeLimit = 5000
)

type GraphNode struct {
	Path  string `json:"path"`
	Type  string `json:"type"`
	Title string `json:"title"`
	// Missing marks a link target no concept holds: named by an agent, never written.
	Missing bool `json:"missing"`
}

type Graph struct {
	Nodes     []GraphNode `json:"nodes"`
	Edges     [][2]string `json:"edges"`
	Truncated bool        `json:"truncated"`
}

// ConsoleService serves the admin console's reads.
type ConsoleService struct {
	r *repositories.ConceptRepository
}

func NewConsoleService(r *repositories.ConceptRepository) *ConsoleService { return &ConsoleService{r} }

func (s *ConsoleService) Tenants(ctx context.Context) ([]models.TenantCount, error) {
	return s.r.Tenants(ctx)
}

func (s *ConsoleService) Totals(ctx context.Context, tenant *uuid.UUID) (models.Totals, error) {
	return s.r.Totals(ctx, tenant)
}

func (s *ConsoleService) DailyWrites(ctx context.Context, tenant *uuid.UUID, days int) ([]models.DailyWrite, error) {
	return s.r.DailyWrites(ctx, tenant, days)
}

func (s *ConsoleService) Page(ctx context.Context, tenant *uuid.UUID, prefix string, limit, offset int) ([]models.Summary, int, error) {
	return s.r.Page(ctx, tenant, prefix, limit, offset)
}

func (s *ConsoleService) Detail(ctx context.Context, tenant uuid.UUID, path string, limit int) (*okf.Concept, []string, []models.Revision, error) {
	return s.r.Detail(ctx, tenant, path, limit)
}

func (s *ConsoleService) Search(ctx context.Context, tenant uuid.UUID, query string, limit int) ([]SearchHit, error) {
	hits, err := s.r.Search(ctx, tenant, query, limit, nil)
	return convert(hits, hitOf), err
}

func (s *ConsoleService) Grep(ctx context.Context, tenant uuid.UUID, pattern string, limit int) ([]GrepHit, error) {
	return grep(ctx, s.r, tenant, pattern, limit)
}

func (s *ConsoleService) Activity(ctx context.Context, tenant *uuid.UUID, limit int) ([]models.Revision, error) {
	return s.r.Activity(ctx, tenant, limit)
}

func (s *ConsoleService) Graph(ctx context.Context, tenant uuid.UUID) (Graph, error) {
	rows, err := s.r.Graph(ctx, tenant, graphLimit+1)
	if err != nil {
		return Graph{}, err
	}
	return buildGraph(rows, graphLimit, graphEdgeLimit), nil
}

func buildGraph(rows []models.GraphRow, nodeLimit, edgeLimit int) Graph {
	g := Graph{Nodes: []GraphNode{}, Edges: [][2]string{}, Truncated: len(rows) > nodeLimit}
	rows = rows[:min(len(rows), nodeLimit)]
	known := map[string]bool{}
	for _, row := range rows {
		known[row.Path] = true
		g.Nodes = append(g.Nodes, GraphNode{Path: row.Path, Type: row.Type, Title: row.Title})
	}
	// Past the row cap, an absent target may just be a concept that did not fit.
	rowsCut := g.Truncated
links:
	for _, row := range rows {
		for _, target := range row.Links {
			if len(g.Edges) >= edgeLimit {
				g.Truncated = true
				break links
			}
			if !known[target] {
				if rowsCut {
					continue
				}
				if len(g.Nodes) >= nodeLimit {
					g.Truncated = true
					continue
				}
				known[target] = true
				g.Nodes = append(g.Nodes, GraphNode{Path: target, Missing: true})
			}
			g.Edges = append(g.Edges, [2]string{row.Path, target})
		}
	}
	return g
}
