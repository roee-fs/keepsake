// The MCP wiring, ported from register in src/keepsake/server/tools.py and the
// streamable HTTP app in src/keepsake/server/app.py.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/roee-fs/keepsake/internal/store"
	"github.com/roee-fs/keepsake/okf"
)

const unavailable = "the knowledge store is temporarily unavailable; try again shortly"

type handler func(ctx context.Context, t *Tools, args map[string]any) (any, error)

// envelope carries a list answer: structured content is object-only through
// protocol 2025-11-25.
type envelope struct {
	Results any `json:"results"`
}

func number(v any) int {
	f, _ := v.(json.Number).Float64()
	return int(f)
}

var handlers = map[string]handler{
	"okf_list": func(ctx context.Context, t *Tools, a map[string]any) (any, error) {
		prefix, _ := a["prefix"].(string)
		return t.List(ctx, prefix)
	},
	"okf_search": func(ctx context.Context, t *Tools, a map[string]any) (any, error) {
		var prefix *string
		if p, ok := a["prefix"].(string); ok {
			prefix = &p
		}
		hits, err := t.Search(ctx, a["query"].(string), number(a["limit"]), prefix)
		return envelope{hits}, err
	},
	"okf_grep": func(ctx context.Context, t *Tools, a map[string]any) (any, error) {
		hits, err := t.Grep(ctx, a["pattern"].(string), number(a["limit"]))
		return envelope{hits}, err
	},
	"okf_read": func(ctx context.Context, t *Tools, a map[string]any) (any, error) {
		c, err := t.Read(ctx, a["path"].(string))
		if c == nil {
			return nil, err
		}
		return c, err
	},
	"okf_create": func(ctx context.Context, t *Tools, a map[string]any) (any, error) {
		return t.Create(ctx, a["path"].(string), a)
	},
	"okf_update": func(ctx context.Context, t *Tools, a map[string]any) (any, error) {
		var expected *int
		if v, ok := a["expected_version"]; ok {
			n := number(v)
			expected = &n
		}
		return t.Update(ctx, a["path"].(string), expected, a)
	},
	"okf_relate": func(ctx context.Context, t *Tools, a map[string]any) (any, error) {
		return t.Relate(ctx, a["from_path"].(string), a["to_path"].(string))
	},
}

// toolList is tools/list as Python answers it: declaration order, and no cacheScope
// or ttlMs, which go-sdk's ListToolsResult always sends.
type toolList struct {
	mcp.ResultBase
	Tools []*mcp.Tool `json:"tools"`
}

// failed is a tool error, not a protocol error: the agent sees it and can correct itself.
func failed(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

func compile(schema any) *jsonschema.Schema {
	b, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		panic(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("schema.json", doc); err != nil {
		panic(err)
	}
	return c.MustCompile("schema.json")
}

var printer = message.NewPrinter(language.English)

// schemaErrors renders every leaf violation as "<path>: <message>", sorted by path
// the way tools.py sorts by json_path.
func schemaErrors(err error) string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err.Error()
	}
	var leaves []string
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			path := "$"
			if len(e.InstanceLocation) > 0 {
				path = strings.Join(e.InstanceLocation, ".")
			}
			leaves = append(leaves, path+": "+e.ErrorKind.LocalizedString(printer))
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	slices.SortStableFunc(leaves, func(a, b string) int {
		pa, _, _ := strings.Cut(a, ": ")
		pb, _, _ := strings.Cut(b, ": ")
		return strings.Compare(pa, pb)
	})
	return strings.Join(leaves, "; ")
}

// arguments decodes the raw arguments keeping frontmatter key order and exact numbers.
func arguments(raw json.RawMessage) (map[string]any, error) {
	m := okf.NewMap()
	if err := json.Unmarshal(raw, m); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, k := range m.Keys() {
		out[k], _ = m.Get(k)
	}
	return out, nil
}

func toolHandler(t *Tools, name string, schema *jsonschema.Schema, call handler) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw := req.Params.Arguments
		if len(raw) == 0 || string(raw) == "null" {
			raw = json.RawMessage("{}")
		}
		instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		// The advertised schema, enforced before dispatch, so a wrong shape comes back
		// as something the agent can correct.
		if err := schema.Validate(instance); err != nil {
			return failed(name + ": " + schemaErrors(err)), nil
		}
		args, err := arguments(raw)
		if err != nil {
			return nil, err
		}
		result, err := call(ctx, t, args)
		var te *ToolError
		switch {
		case errors.As(err, &te):
			return failed(te.Msg), nil
		case store.IsUnavailable(err):
			return failed(unavailable), nil
		case err != nil:
			// A defect here, not the agent's mistake: it stays a protocol error.
			return nil, err
		}
		text, err := pyDumps(result)
		if err != nil {
			return nil, err
		}
		res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
		if result != nil {
			res.StructuredContent = result
		}
		return res, nil
	}
}

// NewMCPHandler serves the seven okf tools over stateless streamable HTTP with JSON responses.
func NewMCPHandler(t *Tools) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "keepsake"}, nil)
	tools := toolDefinitions()
	for _, tool := range tools {
		server.AddTool(tool, toolHandler(t, tool.Name, compile(tool.InputSchema), handlers[tool.Name]))
	}
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				return &toolList{Tools: tools}, nil
			}
			if call, ok := req.(*mcp.CallToolRequest); ok && handlers[call.Params.Name] == nil {
				// An error result, not a protocol error: an agent can correct itself from a
				// tool result and cannot from a transport failure.
				return failed("no such tool: " + call.Params.Name), nil
			}
			return next(ctx, method, req)
		}
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		// A session would pin an agent to one replica; several sit behind one Service.
		Stateless:    true,
		JSONResponse: true,
		// The Host header is a cluster Service name, and no browser can reach the pod,
		// so the localhost-only default would reject every real request. Restore it when
		// auth stops being `none`: this is a setting that outlives its justification.
		DisableLocalhostProtection: true,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// JSON responses never stream, so a client accepting only application/json is
		// served, as the Python server serves it; go-sdk insists on both types.
		r.Header.Add("Accept", "text/event-stream")
		h.ServeHTTP(w, r)
	})
}

// pyDumps renders v as Python's json.dumps does by default: ", " and ": "
// separators and every non-ASCII character escaped.
func pyDumps(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var sb strings.Builder
	var value func() error
	value = func() error {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch tok := tok.(type) {
		case json.Delim:
			sb.WriteRune(rune(tok))
			for i := 0; d.More(); i++ {
				if i > 0 {
					sb.WriteString(", ")
				}
				if tok == '{' {
					k, _ := d.Token()
					pyString(&sb, k.(string))
					sb.WriteString(": ")
				}
				if err := value(); err != nil {
					return err
				}
			}
			end, _ := d.Token()
			sb.WriteRune(rune(end.(json.Delim)))
		case string:
			pyString(&sb, tok)
		case json.Number:
			sb.WriteString(string(tok))
		case bool:
			fmt.Fprint(&sb, tok)
		case nil:
			sb.WriteString("null")
		}
		return nil
	}
	err = value()
	return sb.String(), err
}

func pyString(sb *strings.Builder, s string) {
	sb.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			sb.WriteString(`\"`)
		case r == '\\':
			sb.WriteString(`\\`)
		case r == '\n':
			sb.WriteString(`\n`)
		case r == '\r':
			sb.WriteString(`\r`)
		case r == '\t':
			sb.WriteString(`\t`)
		case r == '\b':
			sb.WriteString(`\b`)
		case r == '\f':
			sb.WriteString(`\f`)
		case r >= ' ' && r <= '~':
			sb.WriteRune(r)
		case r > 0xffff:
			r -= 0x10000
			fmt.Fprintf(sb, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			fmt.Fprintf(sb, `\u%04x`, r)
		}
	}
	sb.WriteByte('"')
}
