// The seven advertised tools, ported from 2de90d2:src/keepsake/server/tools.py's _TOOLS in wire key order.
package server

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/roee-fs/keepsake/okf"
)

// MaxLimit caps and advertises a call's results, so one call cannot return the whole corpus.
const MaxLimit = 200

// MaxVersion is the largest integer a JSON number carries exactly, far below bigint's maximum.
const MaxVersion = 1<<53 - 1

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

// instructions go into the agent's system prompt. On LongMemEval's holdout they pass 49 of 56
// against 44 for bench/variants/team-instructions.json, which cast keepsake as a team knowledge base.
// Change them only with a bench/run.py result: tuned on tune.json, reported on holdout.json.
const instructions = "You have a persistent memory through the keepsake tools. It holds what has been recorded " +
	"before: facts, decisions, history, preferences and past conversations. Before answering anything that may " +
	"depend on it, search it with a few distinctive keywords, read the most relevant results, and follow links " +
	"and backlinks until the answer is grounded. When two memories disagree, prefer the more specific or more " +
	"recent one, and say so. If the memory does not hold the answer, say so rather than guess. Before creating " +
	"a concept, search for an existing one and update it instead if it exists. Name the paths you relied on."

// toolDefinitions is written for an agent reading it cold, with no other documentation.
func toolDefinitions() []*mcp.Tool {
	tools := []*mcp.Tool{
		{
			Name: "list",
			Description: "List the concepts stored here, with a count of each type. Pass `prefix` " +
				"to scope to one part of the tree (`detect/` lists everything beneath " +
				"`detect`); omit it to see everything. This is the cheapest way to learn " +
				"the shape of the knowledge base before searching it.",
			InputSchema: schema(obj("prefix", str())),
		},
		{
			Name: "search",
			Description: "Find concepts by keyword, ranked by relevance. Returns cards — path, " +
				"type, title, description, score, snippet, status, stale, trust, verified_stale, " +
				"generated_at — " +
				"not full concepts; read a promising path with `read`.\n\n" +
				"`snippet` holds the passages of the body that match the query, or is empty " +
				"when only the title or description matched. Use it to choose which paths to read, " +
				"not to answer from: a snippet is cut from its context, so read a path before " +
				"relying on it.\n\n" +
				"`status` is draft, stable or deprecated. `stale` is true once the concept's " +
				"stale_after date has passed. `trust` is unverified, machine-confirmed or " +
				"human-reviewed. `verified_stale` is true when the content changed after the " +
				"verification that sets `trust`, so it is due for review again. `generated_at` " +
				"is when the content last changed, or empty.\n\n" +
				"Matching is lexical, not semantic: the index holds the words that were " +
				"actually written, so distinctive keywords ('dormant', 'PKCE', " +
				"'indextime') find far more than a natural-language question does. Terms " +
				"are OR-ed, so every extra term broadens the result instead of narrowing " +
				"it: add terms to cast wider, drop them to focus. `limit` is required — " +
				"ask for the fewest results you can use. `prefix` confines the search to " +
				"one part of the tree.",
			InputSchema: schema(obj("query", str(), "limit", limit(), "prefix", str()), "query", "limit"),
			OutputSchema: results(schema(
				obj("path", str(), "type", str(), "title", str(), "description", str(), "score", obj("type", "number"),
					"snippet", str(), "status", str(), "stale", obj("type", "boolean"),
					"trust", obj("type", "string", "enum", []string{okf.Unverified, okf.MachineConfirmed, okf.HumanReviewed}),
					"verified_stale", obj("type", "boolean"), "generated_at", str()),
				"path", "type", "title", "description", "score", "snippet", "status", "stale", "trust", "verified_stale",
				"generated_at",
			)),
		},
		{
			Name: "grep",
			Description: "Search concept text with a POSIX regular expression, case-insensitively. " +
				"Returns each matching path with a short snippet around the match. Use it " +
				"when you know the exact string or shape you want — an identifier, a " +
				"config key, a URL — and `search`'s word matching is too loose. " +
				"`limit` is required.",
			InputSchema:  schema(obj("pattern", str(), "limit", limit()), "pattern", "limit"),
			OutputSchema: results(schema(obj("path", str(), "snippet", str()), "path", "snippet")),
		},
		{
			Name: "read",
			Description: "Read one concept in full: body, frontmatter, the concepts it links to, " +
				"and the concepts that link back to it, with the same status, stale, trust, " +
				"verified_stale and generated_at as `search`. Returns null if nothing is stored " +
				"at that path. Take paths from `list`, `search` or `grep`.",
			InputSchema: schema(obj("path", str()), "path"),
		},
		{
			Name: "create",
			Description: "Store a new concept. `path` is relative and carries no `.md` suffix " +
				"(`detect/dormant-rules`). `type` is required and says what kind of thing " +
				"this is — Concept, Runbook, Decision. Fails if the path is taken; change " +
				"an existing concept with `update`.\n\n" +
				"Links are read out of `body`, never declared separately, so relate a " +
				"concept by linking to it inline: `[dormant rules](/detect/dormant-" +
				"rules.md)`. The server sets frontmatter `generated` and `verified` " +
				"itself and ignores yours.",
			InputSchema: schema(withConceptFields("path", str()), "path", "type"),
		},
		{
			Name: "update",
			Description: "Change an existing concept. Fields you leave out keep the values they " +
				"have. Pass `expected_version` (from `read`) to make the write a " +
				"compare-and-swap: if anything has been written since, nothing changes and " +
				"you get back the current version and body to merge against. Leave it out " +
				"only when overwriting whatever is there is acceptable.",
			InputSchema: schema(
				withConceptFields("path", str(), "expected_version", obj("type", "integer", "minimum", 1, "maximum", MaxVersion)),
				"path",
			),
		},
		{
			Name: "relate",
			Description: "Record that one concept relates to another by appending a link from " +
				"`from_path` to `to_path`. The edge then shows up as an outbound link on " +
				"the source and as a backlink on the target.",
			InputSchema: schema(obj("from_path", str(), "to_path", str()), "from_path", "to_path"),
		},
		{
			Name: "verify",
			Description: "Record that you checked a concept against its sources and it holds. " +
				"Pass the `expected_version` you read: if anything has been written since, " +
				"nothing is recorded and you get back the current version and body. Adds " +
				"`{ by: you, at: now }` to frontmatter `verified`, which is the only way " +
				"`verified` changes.",
			InputSchema: schema(
				obj("path", str(), "expected_version", obj("type", "integer", "minimum", 1, "maximum", MaxVersion)),
				"path", "expected_version",
			),
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
