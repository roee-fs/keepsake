package services

import (
	"reflect"
	"testing"

	"github.com/roee-fs/keepsake/internal/models"
)

func paths(g Graph) []string {
	var out []string
	for _, n := range g.Nodes {
		out = append(out, n.Path)
	}
	return out
}

func TestBuildGraphMarksMissingTargets(t *testing.T) {
	g := buildGraph([]models.GraphRow{{Path: "a", Type: "t", Title: "A", Links: []string{"b"}}}, 10, 10)
	if len(g.Nodes) != 2 || g.Nodes[0].Missing || !g.Nodes[1].Missing || g.Truncated {
		t.Fatalf("graph = %+v", g)
	}
}

func TestGraphPastTheCapDropsEdgesItCannotPlace(t *testing.T) {
	g := buildGraph([]models.GraphRow{{Path: "a", Links: []string{"b"}}, {Path: "b"}}, 1, 10)
	// "b" exists past the cap, so it MUST NOT come back as a missing node.
	if !reflect.DeepEqual(paths(g), []string{"a"}) || len(g.Edges) != 0 || !g.Truncated {
		t.Fatalf("graph = %+v", g)
	}
}

func TestGraphCapsMissingTargetsAtTheNodeLimit(t *testing.T) {
	g := buildGraph([]models.GraphRow{{Path: "a", Links: []string{"g1", "g2", "g3", "g4"}}}, 3, 10)
	if !reflect.DeepEqual(paths(g), []string{"a", "g1", "g2"}) ||
		!reflect.DeepEqual(g.Edges, [][2]string{{"a", "g1"}, {"a", "g2"}}) || !g.Truncated {
		t.Fatalf("graph = %+v", g)
	}
}

func TestGraphCapsEdges(t *testing.T) {
	rows := []models.GraphRow{{Path: "a", Links: []string{"b", "c"}}, {Path: "b", Links: []string{"a", "c"}}, {Path: "c"}}
	if g := buildGraph(rows, 10, 2); len(g.Edges) != 2 || !g.Truncated {
		t.Fatalf("graph = %+v", g)
	}
}
