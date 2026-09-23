// The MCP wiring, ported from 2de90d2:src/keepsake/server/tools.py's register and
// 8f2af2e:src/keepsake/server/app.py's streamable HTTP app.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/roee-fs/keepsake/internal/store"
	"github.com/roee-fs/keepsake/okf"
)

const unavailable = "the knowledge store is temporarily unavailable; try again shortly"

type handler func(ctx context.Context, t *Tools, args map[string]any) (any, error)

// envelope carries a list answer, since structured content is object-only through 2025-11-25.
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

// toolList is tools/list without the cacheScope and ttlMs go-sdk's ListToolsResult always sends.
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

// schemaErrors renders each leaf violation as "<path>: <message>", sorted by path as tools.py sorts.
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

// plain is v with every *okf.Map a map[string]any, the tree jsonschema validates.
func plain(v any) any {
	switch v := v.(type) {
	case *okf.Map:
		out := make(map[string]any, v.Len())
		for _, k := range v.Keys() {
			x, _ := v.Get(k)
			out[k] = plain(x)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = plain(x)
		}
		return out
	}
	return v
}

// acquire takes a place in slot, or reports false once ctx ends first.
func acquire(ctx context.Context, slot chan struct{}) (release func(), ok bool) {
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, true
	case <-ctx.Done():
		return nil, false
	}
}

func toolHandler(t *Tools, name string, schema *jsonschema.Schema, call handler, slot chan struct{}) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw := req.Params.Arguments
		if len(raw) == 0 || string(raw) == "null" {
			raw = json.RawMessage("{}")
		}
		// An okf.Map keeps frontmatter key order and exact numbers.
		m := okf.NewMap()
		if err := json.Unmarshal(raw, m); err != nil {
			return nil, err
		}
		// Enforced before dispatch, so a wrong shape is something the agent can correct.
		if err := schema.Validate(plain(m)); err != nil {
			return failed(name + ": " + schemaErrors(err)), nil
		}
		args := map[string]any{}
		for _, k := range m.Keys() {
			args[k], _ = m.Get(k)
		}
		release, ok := acquire(ctx, slot)
		if !ok {
			return nil, ctx.Err()
		}
		defer release()
		result, err := call(ctx, t, args)
		var te *ToolError
		switch {
		case errors.As(err, &te):
			return failed(te.Msg), nil
		case store.IsUnavailable(err):
			return failed(unavailable), nil
		case err != nil:
			// A defect here, not the agent's mistake: it stays a protocol error.
			slog.Error("tool call failed", "tool", name, "err", err)
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
	// Python advertises tools without list-change notifications, and no logging.
	server := mcp.NewServer(&mcp.Implementation{Name: "keepsake"}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})
	tools := toolDefinitions()
	// One fewer than the pool, so a burst queues here and the console keeps a connection.
	slot := make(chan struct{}, max(t.c.PoolSize()-1, 1))
	for _, tool := range tools {
		server.AddTool(tool, toolHandler(t, tool.Name, compile(tool.InputSchema), handlers[tool.Name], slot))
	}
	// Built once per era; each request gets its own result for go-sdk to add _meta to.
	toolsList := map[era]*okf.Map{}
	for _, e := range []era{legacy, modern} {
		m, err := shape("tools/list", e, &toolList{Tools: tools})
		if err != nil {
			panic(err)
		}
		toolsList[e] = m
	}
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			e, _ := ctx.Value(eraKey{}).(era)
			if strings.HasPrefix(method, "notifications/") {
				return next(ctx, method, req)
			}
			if !pythonServes(method, e) {
				return nil, methodNotFound(method)
			}
			if method == "tools/list" {
				return &shaped{m: toolsList[e]}, nil
			}
			var res mcp.Result
			var err error
			if call, ok := req.(*mcp.CallToolRequest); ok && handlers[call.Params.Name] == nil {
				// An error result, which an agent can correct itself from, unlike a protocol error.
				res = failed("no such tool: " + call.Params.Name)
			} else if res, err = next(ctx, method, req); err != nil || shapes[method] == nil {
				return res, err
			}
			m, err := shape(method, e, res)
			if err != nil {
				return nil, err
			}
			return &shaped{m: m}, nil
		}
	})
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		// A session would pin an agent to one replica; several sit behind one Service.
		Stateless:    true,
		JSONResponse: true,
		// The Host header is a cluster Service name; refuseBrowsers covers DNS rebinding.
		DisableLocalhostProtection: true,
	})
	// Python's RequestBodyLimitMiddleware runs before everything else, at go-sdk's limit.
	return limitBody(mcp.DefaultMaxRequestBodyBytes, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e := eraOf(r)
		if r.Method != http.MethodPost && r.Method != http.MethodGet && r.Method != http.MethodHead || r.Method == http.MethodGet && e != legacy {
			methodNotAllowed(w, r.Method, e)
			return
		}
		// TransportSecurityMiddleware checks this first, even with rebinding protection off.
		if r.Method == http.MethodPost && !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			// Starlette's bare Response sends no Content-Type; nil stops net/http sniffing one.
			w.Header()["Content-Type"] = nil
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, "Invalid Content-Type header")
			return
		}
		if r.Method == http.MethodPost && !acceptsJSON(r.Header.Values("Accept")) {
			notAcceptable(w, e)
			return
		}
		if r.Method == http.MethodPost && rejected(w, r, e) {
			return
		}
		// go-sdk insists on both types; Python serves an application/json-only client, since JSON never streams.
		r.Header.Add("Accept", "text/event-stream")
		sdk.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), eraKey{}, e)))
	}))
}

// acceptsJSON is check_accept_headers in mcp/server/streamable_http.py.
func acceptsJSON(accept []string) bool {
	for _, part := range strings.Split(strings.Join(accept, ","), ",") {
		mt, _, _ := strings.Cut(part, ";")
		switch strings.ToLower(strings.TrimSpace(mt)) {
		case "application/json", "application/*", "*/*":
			return true
		}
	}
	return false
}

// tooLarge is Starlette's bare 413 for a body over the limit.
func tooLarge(w http.ResponseWriter) {
	w.Header()["Content-Type"] = nil
	w.WriteHeader(http.StatusRequestEntityTooLarge)
	io.WriteString(w, "Request body too large")
}

// methodNotAllowed answers as the Python transport would; a legacy GET is left to go-sdk.
func methodNotAllowed(w http.ResponseWriter, method string, e era) {
	if e != legacy {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	msg := "Method Not Allowed: Session termination not supported"
	if method != http.MethodDelete {
		msg = "Method Not Allowed"
		w.Header().Set("Allow", "GET, POST, DELETE")
	}
	rpcError(w, http.StatusMethodNotAllowed, nil, codeInvalidReq, msg)
}

// notAcceptable answers 406 as whichever Python transport the request reaches.
func notAcceptable(w http.ResponseWriter, e era) {
	if e != legacy {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	rpcError(w, http.StatusNotAcceptable, nil, codeInvalidReq, "Not Acceptable: Client must accept application/json")
}

// pyDumps renders v as Python's default json.dumps: ", " and ": " separators, non-ASCII escaped.
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
			r1, r2 := utf16.EncodeRune(r)
			fmt.Fprintf(sb, `\u%04x\u%04x`, r1, r2)
		default:
			fmt.Fprintf(sb, `\u%04x`, r)
		}
	}
	sb.WriteByte('"')
}
