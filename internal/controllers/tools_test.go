// Ported from 2de90d2:tests/test_tools.py. The app-level tests there (readyz, build_app)
// belong to the serve wiring, not to the tools.
package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/roee-fs/keepsake/internal/migrate"
	"github.com/roee-fs/keepsake/internal/pgtest"
	"github.com/roee-fs/keepsake/internal/repositories"
	"github.com/roee-fs/keepsake/internal/services"
	"github.com/roee-fs/keepsake/okf"
)

var (
	db  *pgtest.DB
	ctx = context.Background()
)

// secret is distinctive enough that its appearance anywhere is proof, not coincidence.
const secret = "zqxjkbody"

var toolNames = []string{"create", "grep", "list", "read", "relate", "search", "update", "verify"}

func TestMain(m *testing.M) {
	pgtest.Main(m, func(d *pgtest.DB) error {
		db = d
		return migrate.Up(ctx, d.OwnerDSN, "okf")
	})
}

func conceptStore(t *testing.T) *repositories.ConceptRepository {
	return pgtest.ConceptRepository(t, db.AppDSN)
}

func newTools(t *testing.T) *services.ConceptService {
	t.Helper()
	return services.NewConceptService(conceptStore(t), uuid.New(), "mcp")
}

func connect(t *testing.T, tools *services.ConceptService) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(NewMCPHandler(tools))
	t.Cleanup(srv.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "keepsake-test"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	return res.Content[0].(*mcp.TextContent).Text
}

func listTools(t *testing.T) map[string]*mcp.Tool {
	t.Helper()
	res, err := connect(t, newTools(t)).ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		out[tool.Name] = tool
	}
	return out
}

// field reads one key out of a schema the client decoded as a generic JSON value.
func field(schema any, key string) any { return schema.(map[string]any)[key] }

func TestTheServerAdvertisesExactlyTheEightTools(t *testing.T) {
	if names := slices.Sorted(maps.Keys(listTools(t))); !reflect.DeepEqual(names, toolNames) {
		t.Fatalf("advertised %v", names)
	}
}

func TestSearchAndGrepAdvertiseLimitAsRequired(t *testing.T) {
	advertised := listTools(t)
	for _, name := range []string{"search", "grep"} {
		required := field(advertised[name].InputSchema, "required").([]any)
		if !slices.Contains(required, any("limit")) {
			t.Fatalf("%s required = %v", name, required)
		}
	}
}

func TestSearchAndGrepAdvertiseTheEnvelopeTheyAnswerIn(t *testing.T) {
	advertised := listTools(t)
	for _, name := range []string{"search", "grep"} {
		results := field(field(advertised[name].OutputSchema, "properties"), "results")
		if field(results, "type") != "array" {
			t.Fatalf("%s results = %v", name, results)
		}
	}
}

func TestEveryToolIsDescribedAndSearchExplainsItsMatching(t *testing.T) {
	advertised := listTools(t)
	for name, tool := range advertised {
		if tool.Description == "" {
			t.Fatalf("%s has no description", name)
		}
	}
	search := strings.ToLower(advertised["search"].Description)
	for _, word := range []string{"lexical", "or-ed", "distinctive"} {
		if !strings.Contains(search, word) {
			t.Fatalf("search description lacks %q", word)
		}
	}
}

func TestAToolCallRoundTripsOverTheProtocol(t *testing.T) {
	session := connect(t, newTools(t))
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "create", Arguments: map[string]any{
		"path": "e2e/smoke", "type": "Concept", "title": "Smoke", "body": secret,
	}}); err != nil {
		t.Fatal(err)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{"query": "smoke", "limit": 5}})
	if err != nil {
		t.Fatal(err)
	}
	matched, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "grep", Arguments: map[string]any{"pattern": "smoke", "limit": 5}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || matched.IsError {
		t.Fatalf("errors: %v %v", result.IsError, matched.IsError)
	}
	hits := field(result.StructuredContent, "results").([]any)
	if len(hits) != 1 || field(hits[0], "path") != "e2e/smoke" {
		t.Fatalf("hits = %v", hits)
	}
	if field(hits[0], "trust") != okf.Unverified || field(hits[0], "status") != "stable" {
		t.Fatalf("card = %v", hits[0])
	}
	if err := compile(listTools(t)["search"].OutputSchema).Validate(result.StructuredContent); err != nil {
		t.Fatalf("result breaks the advertised output schema: %v", err)
	}
	structured, _ := json.Marshal(result.StructuredContent)
	if strings.Contains(string(structured), secret) || strings.Contains(text(t, result), secret) {
		t.Fatal("leaked a body")
	}
	var fromText any
	if err := json.Unmarshal([]byte(text(t, result)), &fromText); err != nil || !reflect.DeepEqual(fromText, result.StructuredContent) {
		t.Fatalf("text %q disagrees with structured content", text(t, result))
	}
}

func TestARejectedCallIsAnErrorResultNotAProtocolError(t *testing.T) {
	res, err := connect(t, newTools(t)).CallTool(ctx, &mcp.CallToolParams{Name: "create", Arguments: map[string]any{"path": "a/b", "type": ""}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(text(t, res), "type is required") {
		t.Fatalf("got IsError=%v %q", res.IsError, text(t, res))
	}
}

func TestACallMissingARequiredArgumentIsAnErrorResult(t *testing.T) {
	res, err := connect(t, newTools(t)).CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{"query": "smoke"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("IsError = false")
	}
}

// A defect below the boundary MUST stay a defect: a tidy error result would send the
// agent to retry a request that was never wrong, and hide the bug.
func TestAnInternalErrorIsNotDressedUpAsTheAgentsMistake(t *testing.T) {
	session := connect(t, newTools(t))
	pgtest.Exec(t, db.AdminDSN, `REVOKE SELECT ON okf.concept FROM keepsake_app`)
	t.Cleanup(func() { pgtest.Exec(t, db.AdminDSN, `GRANT SELECT ON okf.concept TO keepsake_app`) })
	if res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list", Arguments: map[string]any{}}); err == nil {
		t.Fatalf("got a result, want a protocol error: %+v", res)
	}
}

func TestADefectIsLoggedAndAnAgentMistakeIsNot(t *testing.T) {
	var logs bytes.Buffer
	orig := slog.Default()
	// Warn, since every call also logs an Info line.
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	session := connect(t, newTools(t))

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"path": "../x"}}); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Fatalf("a tool error was logged: %s", logs.String())
	}
	pgtest.Exec(t, db.AdminDSN, `REVOKE SELECT ON okf.concept FROM keepsake_app`)
	t.Cleanup(func() { pgtest.Exec(t, db.AdminDSN, `GRANT SELECT ON okf.concept TO keepsake_app`) })
	session.CallTool(ctx, &mcp.CallToolParams{Name: "list", Arguments: map[string]any{}})
	if !strings.Contains(logs.String(), "list") || !strings.Contains(logs.String(), "permission denied") {
		t.Fatalf("the defect was not logged: %q", logs.String())
	}
}

// A burst past the pool queues for a connection rather than timing out on one.
func TestABurstPastThePoolQueuesInsteadOfTimingOut(t *testing.T) {
	t.Setenv("KEEPSAKE_POOL_SIZE", "2")
	session := connect(t, newTools(t))
	defer repositories.SetAcquireTimeout(100 * time.Millisecond)()

	conn, err := pgx.Connect(ctx, db.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE okf.concept IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan string, 6)
	for range 6 {
		wg.Go(func() {
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list", Arguments: map[string]any{}})
			switch {
			case err != nil:
				errs <- err.Error()
			case res.IsError:
				errs <- res.Content[0].(*mcp.TextContent).Text
			}
		})
	}
	time.Sleep(300 * time.Millisecond)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// Plausible from an LLM, and a protocol error would give it nothing to act on.
func TestAnArgumentOfTheWrongShapeIsAnErrorResult(t *testing.T) {
	res, err := connect(t, newTools(t)).CallTool(ctx, &mcp.CallToolParams{Name: "create", Arguments: map[string]any{
		"path": "a/b", "type": "Concept", "frontmatter": "owner: sec",
	}})
	if err != nil {
		t.Fatal(err)
	}
	// The wording is santhosh-tekuri/jsonschema's, not Python's, but it still names
	// the field and the type it wanted.
	msg := text(t, res)
	if !res.IsError || !strings.Contains(msg, "frontmatter") || !strings.Contains(msg, "object") {
		t.Fatalf("got IsError=%v %q", res.IsError, msg)
	}
}

func TestSchemaErrorsAreSortedAndNameTheTool(t *testing.T) {
	res, err := connect(t, newTools(t)).CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{
		"query": 1, "limit": 201, "links": []any{},
	}})
	if err != nil {
		t.Fatal(err)
	}
	msg := text(t, res)
	if !res.IsError || !strings.HasPrefix(msg, "search: $: ") ||
		strings.Index(msg, "; limit: ") > strings.Index(msg, "; query: ") {
		t.Fatalf("got IsError=%v %q", res.IsError, msg)
	}
}

// New: tools.py answers an unknown name with an error result, not a protocol error.
func TestAnUnknownToolIsAnErrorResult(t *testing.T) {
	res, err := connect(t, newTools(t)).CallTool(ctx, &mcp.CallToolParams{Name: "delete", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || text(t, res) != "no such tool: delete" {
		t.Fatalf("got IsError=%v %q", res.IsError, text(t, res))
	}
}

// New: the text content is json.dumps of the structured content, byte for byte.
func TestTextContentIsPythonJSONDumps(t *testing.T) {
	session := connect(t, newTools(t))
	created, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "create", Arguments: map[string]any{
		"path": "a/b", "type": "Concept", "body": "café \"<&>\"\n", "frontmatter": map[string]any{"n": 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"path": "a/b"}})
	if err != nil {
		t.Fatal(err)
	}
	at := field(field(field(res.StructuredContent, "frontmatter"), "generated"), "at")
	want := `{"path": "a/b", "type": "Concept", "title": "", "description": "", "body": "caf\u00e9 \"<&>\"\n", ` +
		fmt.Sprintf(`"frontmatter": {"n": 1, "generated": {"at": %q, "by": "mcp"}}, "version": %v, "links": [], "backlinks": [], `+
			`"status": "stable", "stale": false, "trust": "unverified", "generated_at": %q}`,
			at, field(created.StructuredContent, "version"), at)
	if got := text(t, res); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"path": "no/such"}})
	if err != nil {
		t.Fatal(err)
	}
	if text(t, res) != "null" || res.StructuredContent != nil {
		t.Fatalf("got %q %v", text(t, res), res.StructuredContent)
	}
}

// postToolsList sends tools/list the way a Service in front of the pod does: no
// handshake, no session header, no SSE. It returns the raw JSON-RPC result.
func postToolsList(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	srv := httptest.NewServer(NewMCPHandler(newTools(t)))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ Result map[string]json.RawMessage }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("status %d: %v", resp.StatusCode, err)
	}
	return body.Result
}

func TestTheEndpointAnswersAPlainJSONPost(t *testing.T) {
	var tools []struct{ Name string }
	if err := json.Unmarshal(postToolsList(t)["tools"], &tools); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if !reflect.DeepEqual(names, toolNames) {
		t.Fatalf("advertised %v", names)
	}
}

// The legacy wire, not what toolDefinitions returns: go-sdk alone would sort the
// tools by name and add cache fields that Python's legacy listing does not carry.
func TestVerifyIsCallableOverTheProtocol(t *testing.T) {
	tools := newTools(t)
	created, err := tools.Create(ctx, "a/b", map[string]any{"type": "Concept"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := connect(t, tools).CallTool(ctx, &mcp.CallToolParams{
		Name: "verify", Arguments: map[string]any{"path": "a/b", "expected_version": created.Version},
	})
	if err != nil || res.IsError {
		t.Fatalf("got %+v, %v", res, err)
	}
	if c, err := tools.Read(ctx, "a/b"); err != nil || c.Trust != okf.MachineConfirmed {
		t.Fatalf("read %+v, %v", c, err)
	}
}

func TestTheWireToolListingIsPythons(t *testing.T) {
	result := postToolsList(t)
	if keys := slices.Sorted(maps.Keys(result)); !reflect.DeepEqual(keys, []string{"tools"}) {
		t.Fatalf("result keys = %v, want exactly [tools]", keys)
	}
	var tools []struct{ Name string }
	if err := json.Unmarshal(result["tools"], &tools); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	want := []string{"list", "search", "grep", "read", "create", "update", "relate", "verify"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("advertised %v, want %v", names, want)
	}
	golden, _ := os.ReadFile("testdata/tools.json")
	if !jsonEqualOrdered(result["tools"], golden) {
		t.Fatalf("wire tools drifted from testdata/tools.json:\n%s", result["tools"])
	}
}

func TestUnavailableIsAToolError(t *testing.T) {
	session := connect(t, newTools(t)) // a go-sdk client session over httptest
	// Terminating backends alone is not enough: the pre-acquire ping would reconnect.
	db.LockOut(t)

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("a database that is down MUST be a tool result, not a protocol error: %v", err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !res.IsError || text != "the knowledge store is temporarily unavailable; try again shortly" {
		t.Fatalf("got IsError=%v %q", res.IsError, text)
	}
}

// The wants are the Python server's answers: a legacy request gets a JSON-RPC error,
// a modern one (a non-handshake mcp-protocol-version) an empty 406.
func TestAnUnacceptableAcceptIsPythons406(t *testing.T) {
	srv := httptest.NewServer(NewMCPHandler(newTools(t)))
	defer srv.Close()
	for _, tc := range []struct{ accept, version, wantType, wantBody string }{
		{"text/html", "", "application/json", `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Not Acceptable: Client must accept application/json"}}`},
		{"text/event-stream", "2025-11-25", "application/json", `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Not Acceptable: Client must accept application/json"}}`},
		{"text/html", "2026-07-28", "", ""},
	} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", tc.accept)
		if tc.version != "" {
			req.Header.Set("Mcp-Protocol-Version", tc.version)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotAcceptable || resp.Header.Get("Content-Type") != tc.wantType || string(body) != tc.wantBody {
			t.Errorf("%q %q: %d %q %s", tc.accept, tc.version, resp.StatusCode, resp.Header.Get("Content-Type"), body)
		}
	}
}

// wireCase is one entry of testdata/python_wire.json: requests and the result the Python server
// (2de90d2) answered them with, on an empty tenant.
type wireCase struct {
	Name    string
	Headers map[string]string
	Request json.RawMessage
	Result  json.RawMessage
}

// wireResults posts each named case to the Go handler and returns the golden and got results.
func wireResults(t *testing.T, names ...string) map[string][2]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("testdata/python_wire.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []wireCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMCPHandler(newTools(t)))
	defer srv.Close()
	out := map[string][2]json.RawMessage{}
	for _, c := range cases {
		if len(names) > 0 && !slices.Contains(names, c.Name) {
			continue
		}
		req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(c.Request))
		for k, v := range c.Headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body struct{ Result json.RawMessage }
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("%s: status %d: %v", c.Name, resp.StatusCode, err)
		}
		out[c.Name] = [2]json.RawMessage{c.Result, body.Result}
	}
	return out
}

func TestModernToolsListIsPythons(t *testing.T) {
	for name, r := range wireResults(t, "modern tools/list") {
		if !reflect.DeepEqual(tokens(r[1]), tokens(r[0])) {
			t.Errorf("%s:\n got %s\nwant %s", name, r[1], r[0])
		}
	}
}

func TestSuccessWireIsPythons(t *testing.T) {
	for name, r := range wireResults(t) {
		if !reflect.DeepEqual(tokens(r[1]), tokens(r[0])) {
			t.Errorf("%s:\n got %s\nwant %s", name, r[1], r[0])
		}
	}
}

// The wants are the Python server's answers to methods other than POST.
func TestOtherMethodsArePythons405(t *testing.T) {
	srv := httptest.NewServer(NewMCPHandler(newTools(t)))
	defer srv.Close()
	for _, tc := range []struct{ method, version, allow, body string }{
		{http.MethodDelete, "", "", `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Method Not Allowed: Session termination not supported"}}`},
		{http.MethodPut, "", "GET, POST, DELETE", `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Method Not Allowed"}}`},
		{http.MethodDelete, "2026-07-28", "POST", ""},
		{http.MethodGet, "2026-07-28", "POST", ""},
		// Python's transport answers HEAD as any other unsupported method; a HEAD response has no body.
		{http.MethodHead, "", "GET, POST, DELETE", ""},
		{http.MethodHead, "2026-07-28", "POST", ""},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL, nil)
		if tc.version != "" {
			req.Header.Set("Mcp-Protocol-Version", tc.version)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != tc.allow || string(body) != tc.body {
			t.Errorf("%s %q: %d allow %q %s", tc.method, tc.version, resp.StatusCode, resp.Header.Get("Allow"), body)
		}
	}
}

// The wants are the Python server's status and JSON-RPC error code for the same
// request; the message text MAY differ from Python's. Code 0 means no JSON-RPC error body,
// and -1 a result.
func TestProtocolErrorsArePythons(t *testing.T) {
	srv := httptest.NewServer(NewMCPHandler(newTools(t)))
	defer srv.Close()
	const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	const badMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"1999-01-01","io.modelcontextprotocol/clientCapabilities":{}}`
	legacy := map[string]string{"Mcp-Protocol-Version": "2025-11-25"}
	modern := func(method string, extra ...string) map[string]string {
		h := map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": method}
		for i := 0; i < len(extra); i += 2 {
			h[extra[i]] = extra[i+1]
		}
		return h
	}
	for _, tc := range []struct {
		name    string
		headers map[string]string
		body    string
		status  int
		code    int
	}{
		{"legacy parse", legacy, `{`, 400, -32700},
		{"legacy scalar", legacy, `"x"`, 400, -32602},
		{"legacy null", legacy, `null`, 400, -32602},
		{"legacy batch", legacy, `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`, 400, -32602},
		{"legacy empty batch", legacy, `[]`, 400, -32602},
		{"legacy jsonrpc 1.0", legacy, `{"jsonrpc":"1.0","id":1,"method":"tools/list"}`, 400, -32602},
		{"legacy method not a string", legacy, `{"jsonrpc":"2.0","id":1,"method":5}`, 400, -32602},
		{"legacy notification params array", legacy, `{"jsonrpc":"2.0","method":"x","params":[1]}`, 400, -32602},
		{"legacy result array", legacy, `{"jsonrpc":"2.0","id":1,"result":[]}`, 400, -32602},
		{"legacy id bool", legacy, `{"jsonrpc":"2.0","id":true,"method":"tools/list"}`, 202, 0},
		{"legacy id float", legacy, `{"jsonrpc":"2.0","id":1.5,"method":"tools/list"}`, 202, 0},
		{"legacy id null", legacy, `{"jsonrpc":"2.0","id":null,"method":"tools/list"}`, 202, 0},
		{"legacy unknown method", legacy, `{"jsonrpc":"2.0","id":1,"method":"foo/bar"}`, 200, -32601},
		{"legacy unknown notification", legacy, `{"jsonrpc":"2.0","method":"foo/bar"}`, 202, 0},
		{"legacy resources/list", legacy, `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, 200, -32601},
		{"legacy prompts/list", legacy, `{"jsonrpc":"2.0","id":1,"method":"prompts/list"}`, 200, -32601},
		{"legacy logging/setLevel", legacy, `{"jsonrpc":"2.0","id":1,"method":"logging/setLevel","params":{"level":"info"}}`, 200, -32601},
		{"legacy initialize without params", legacy, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, 200, -32602},
		{"legacy tools/call without name", legacy, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`, 200, -32602},
		{"legacy tools/call arguments array", legacy, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list","arguments":[1]}}`, 200, -32602},
		{"legacy ping", legacy, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, 200, -1},
		{"text content type", map[string]string{"Content-Type": "text/plain"}, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 400, 0},
		{"modern parse", modern("tools/list"), `{`, 400, -32700},
		{"modern scalar", modern("tools/list"), `"x"`, 400, -32600},
		{"modern batch", modern("tools/list"), `[{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + meta + `}}]`, 400, -32600},
		{"modern jsonrpc 1.0", modern("tools/list"), `{"jsonrpc":"1.0","id":1,"method":"tools/list","params":{` + meta + `}}`, 400, -32600},
		{"modern tools/call without name", modern("tools/call"), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{` + meta + `}}`, 400, -32602},
		{"modern tools/call arguments array", modern("tools/call", "Mcp-Name", "list"), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list","arguments":[1],` + meta + `}}`, 400, -32602},
		{"modern resources/list", modern("resources/list"), `{"jsonrpc":"2.0","id":1,"method":"resources/list","params":{` + meta + `}}`, 404, -32601},
		{"modern unknown method", modern("foo/bar"), `{"jsonrpc":"2.0","id":1,"method":"foo/bar","params":{` + meta + `}}`, 404, -32601},
		{"modern ping", modern("ping"), `{"jsonrpc":"2.0","id":1,"method":"ping","params":{` + meta + `}}`, 404, -32601},
		{"modern method mismatch", modern("tools/call"), `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + meta + `}}`, 400, -32020},
		{"modern missing _meta", modern("tools/list"), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 400, -32602},
		{"unknown version without _meta", map[string]string{"Mcp-Protocol-Version": "1999-01-01"}, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 400, -32602},
		{"unknown version in _meta", modern("tools/list", "Mcp-Protocol-Version", "1999-01-01"), `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + badMeta + `}}`, 400, -32022},
		{"unknown header, modern _meta", modern("tools/list", "Mcp-Protocol-Version", "1999-01-01"), `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + meta + `}}`, 400, -32020},
		{"empty version header", map[string]string{"Mcp-Protocol-Version": ""}, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 400, -32602},
	} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range tc.headers {
			req.Header[http.CanonicalHeaderKey(k)] = []string{v}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var body struct {
			Result json.RawMessage
			Error  *struct{ Code int }
		}
		code := 0
		if json.Unmarshal(raw, &body) == nil {
			switch {
			case body.Error != nil:
				code = body.Error.Code
			case body.Result != nil:
				code = -1
			}
		}
		if resp.StatusCode != tc.status || code != tc.code {
			t.Errorf("%s: got %d %d, want %d %d: %s", tc.name, resp.StatusCode, code, tc.status, tc.code, raw)
		}
	}
}

func TestAContentTypeRefusalIsPythons(t *testing.T) {
	srv := httptest.NewServer(NewMCPHandler(newTools(t)))
	defer srv.Close()
	resp, err := http.Post(srv.URL, "text/plain", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if _, set := resp.Header["Content-Type"]; resp.StatusCode != 400 || set || string(body) != "Invalid Content-Type header" {
		t.Fatalf("%d %v %q", resp.StatusCode, resp.Header, body)
	}
}

// countingReader serves n bytes of a JSON string body and counts what was read.
type countingReader struct{ n, read int64 }

func (c *countingReader) Read(p []byte) (int, error) {
	if c.read >= c.n {
		return 0, io.EOF
	}
	k := min(int64(len(p)), c.n-c.read)
	for i := range p[:k] {
		p[i] = ' '
	}
	c.read += k
	return int(k), nil
}

// Python's RequestBodyLimitMiddleware answers a body over 4 MiB with a bare 413, and
// the handler MUST stop reading at the limit rather than buffer the whole body.
func TestAnOversizedBodyIsRefusedAtTheLimit(t *testing.T) {
	h := NewMCPHandler(newTools(t))
	for _, declared := range []bool{false, true} {
		body := &countingReader{n: mcp.DefaultMaxRequestBodyBytes + 1<<20}
		req := httptest.NewRequest(http.MethodPost, "/", body)
		req.ContentLength = -1
		if declared {
			req.ContentLength = body.n
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if set := rec.Header().Values("Content-Type"); rec.Code != http.StatusRequestEntityTooLarge || len(set) > 0 || rec.Body.String() != "Request body too large" {
			t.Errorf("declared=%v: %d %v %q", declared, rec.Code, rec.Header(), rec.Body)
		}
		if body.read > mcp.DefaultMaxRequestBodyBytes+1 {
			t.Errorf("declared=%v: read %d bytes, past the %d limit", declared, body.read, mcp.DefaultMaxRequestBodyBytes)
		}
	}
}

// testdata/python_protocol_errors.json is the Python server's answer to each request,
// byte for byte, captured from 8f2af2e's app on mcp 2.2.0.
func TestProtocolErrorBytesArePythons(t *testing.T) {
	raw, err := os.ReadFile("testdata/python_protocol_errors.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Body, ContentType, Response string
		Headers                           [][2]string
		Status                            int
	}
	if err := json.Unmarshal(raw, &cases); err != nil || len(cases) == 0 {
		t.Fatalf("%d cases: %v", len(cases), err)
	}
	srv := httptest.NewServer(NewMCPHandler(newTools(t)))
	defer srv.Close()
	for _, tc := range cases {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(tc.Body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for _, h := range tc.Headers {
			req.Header.Add(h[0], h[1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.Status || resp.Header.Get("Content-Type") != tc.ContentType || string(body) != tc.Response {
			t.Errorf("%s:\n got %d %q %s\nwant %d %q %s", tc.Name, resp.StatusCode, resp.Header.Get("Content-Type"), body, tc.Status, tc.ContentType, tc.Response)
		}
	}
}
