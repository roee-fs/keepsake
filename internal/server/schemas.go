// The seven advertised tools, ported from 2de90d2:src/keepsake/server/tools.py's _TOOLS in wire key order.
package server

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/roee-fs/keepsake/okf"
)

// MaxLimit caps and advertises a call's results, so one call cannot return the whole corpus.
const MaxLimit = 200

// MaxVersion is int4's maximum, so expected_version never overflows into a database error.
const MaxVersion = 2147483647

// obj builds an ordered JSON object from key, value pairs.
func obj(kv ...any) *okf.Map {
	m := okf.NewMap()
	for i := 0; i < len(kv); i += 2 {
		m.Set(kv[i].(string), kv[i+1])
	}
	return m
}

func str() *okf.Map { return obj("type", "string") }

// schema is closed: an unread argument is a caller believing something took effect.
func schema(properties *okf.Map, required ...string) *okf.Map {
	if required == nil {
		required = []string{}
	}
	return obj("type", "object", "properties", properties, "required", required, "additionalProperties", false)
}

// results is the envelope a list answer travels in.
func results(item *okf.Map) *okf.Map {
	return schema(obj("results", obj("type", "array", "items", item)), "results")
}

func limit() *okf.Map { return obj("type", "integer", "minimum", 0, "maximum", MaxLimit) }

// conceptFields are the fields a write sets, in the order tools.py declares them.
var conceptFields = []string{"type", "title", "description", "body", "frontmatter"}

func withConceptFields(kv ...any) *okf.Map {
	m := obj(kv...)
	for _, f := range conceptFields {
		if f == "frontmatter" {
			m.Set(f, obj("type", "object"))
		} else {
			m.Set(f, str())
		}
	}
	return m
}

// instructions go into the agent's system prompt. On LongMemEval's holdout they pass 48 of 56
// against 42 for bench/variants/team-instructions.json, which cast keepsake as a team knowledge base.
// Change them only with a bench/run.py result: tuned on tune.json, reported on holdout.json.
const instructions = "You have a persistent memory through the okf_* tools. It holds what has been recorded " +
	"before: facts, decisions, history, preferences and past conversations. Before answering anything that may " +
	"depend on it, search it with a few distinctive keywords, read the most relevant results, and follow links " +
	"and backlinks until the answer is grounded. When two memories disagree, prefer the more specific or more " +
	"recent one, and say so. If the memory does not hold the answer, say so rather than guess. Before creating " +
	"a concept, search for an existing one and update it instead if it exists. Name the paths you relied on."

// toolDefinitions is written for an agent reading it cold, with no other documentation.
func toolDefinitions() []*mcp.Tool {
	tools := []*mcp.Tool{
		{
			Name: "okf_list",
			Description: "List the concepts stored here, with a count of each type. Pass `prefix` " +
				"to scope to one part of the tree (`detect/` lists everything beneath " +
				"`detect`); omit it to see everything. This is the cheapest way to learn " +
				"the shape of the knowledge base before searching it.",
			InputSchema: schema(obj("prefix", str())),
		},
		{
			Name: "okf_search",
			Description: "Find concepts by keyword, ranked by relevance. Returns cards — path, " +
				"type, title, description, score — and never a body; read a promising " +
				"path with okf_read.\n\n" +
				"Matching is lexical, not semantic: the index holds the words that were " +
				"actually written, so distinctive keywords ('dormant', 'PKCE', " +
				"'indextime') find far more than a natural-language question does. Terms " +
				"are OR-ed, so every extra term broadens the result instead of narrowing " +
				"it: add terms to cast wider, drop them to focus. `limit` is required — " +
				"ask for the fewest results you can use. `prefix` confines the search to " +
				"one part of the tree.",
			InputSchema: schema(obj("query", str(), "limit", limit(), "prefix", str()), "query", "limit"),
			OutputSchema: results(schema(
				obj("path", str(), "type", str(), "title", str(), "description", str(), "score", obj("type", "number")),
				"path", "type", "title", "description", "score",
			)),
		},
		{
			Name: "okf_grep",
			Description: "Search concept text with a POSIX regular expression, case-insensitively. " +
				"Returns each matching path with a short snippet around the match. Use it " +
				"when you know the exact string or shape you want — an identifier, a " +
				"config key, a URL — and okf_search's word matching is too loose. " +
				"`limit` is required.",
			InputSchema:  schema(obj("pattern", str(), "limit", limit()), "pattern", "limit"),
			OutputSchema: results(schema(obj("path", str(), "snippet", str()), "path", "snippet")),
		},
		{
			Name: "okf_read",
			Description: "Read one concept in full: body, frontmatter, the concepts it links to, " +
				"and the concepts that link back to it. Returns null if nothing is stored " +
				"at that path. Take paths from okf_list, okf_search or okf_grep.",
			InputSchema: schema(obj("path", str()), "path"),
		},
		{
			Name: "okf_create",
			Description: "Store a new concept. `path` is relative and carries no `.md` suffix " +
				"(`detect/dormant-rules`). `type` is required and says what kind of thing " +
				"this is — Concept, Runbook, Decision. Fails if the path is taken; change " +
				"an existing concept with okf_update.\n\n" +
				"Links are read out of `body`, never declared separately, so relate a " +
				"concept by linking to it inline: `[dormant rules](/detect/dormant-" +
				"rules.md)`.",
			InputSchema: schema(withConceptFields("path", str()), "path", "type"),
		},
		{
			Name: "okf_update",
			Description: "Change an existing concept. Fields you leave out keep the values they " +
				"have. Pass `expected_version` (from okf_read) to make the write a " +
				"compare-and-swap: if anything has been written since, nothing changes and " +
				"you get back the current version and body to merge against. Leave it out " +
				"only when overwriting whatever is there is acceptable.",
			InputSchema: schema(
				withConceptFields("path", str(), "expected_version", obj("type", "integer", "minimum", 1, "maximum", MaxVersion)),
				"path",
			),
		},
		{
			Name: "okf_relate",
			Description: "Record that one concept relates to another by appending a link from " +
				"`from_path` to `to_path`. The edge then shows up as an outbound link on " +
				"the source and as a backlink on the target.",
			InputSchema: schema(obj("from_path", str(), "to_path", str()), "from_path", "to_path"),
		},
	}
	// Properties, required and type lead a top-level schema, as mcp_types serialises it.
	for _, t := range tools {
		t.InputSchema = first(t.InputSchema.(*okf.Map), "properties", "required", "type")
		if t.OutputSchema != nil {
			t.OutputSchema = first(t.OutputSchema.(*okf.Map), "properties", "required", "type")
		}
	}
	return tools
}
