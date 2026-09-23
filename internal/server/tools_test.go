// Ported from tests/test_tools.py. The app-level tests there (readyz, build_app)
// belong to the serve wiring, not to the tools.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/roee-fs/keepsake/internal/migrate"
	"github.com/roee-fs/keepsake/internal/pgtest"
	"github.com/roee-fs/keepsake/internal/store"
	"github.com/roee-fs/keepsake/okf"
)

var (
	db  *pgtest.DB
	ctx = context.Background()
)

// secret is distinctive enough that its appearance anywhere is proof, not coincidence.
const secret = "zqxjkbody"

var toolNames = []string{"okf_create", "okf_grep", "okf_list", "okf_read", "okf_relate", "okf_search", "okf_update"}

func TestMain(m *testing.M) {
	d, cleanup, err := pgtest.Start(ctx)
	if err != nil {
		panic(err)
	}
	if err := migrate.Up(ctx, d.OwnerDSN, "okf"); err != nil {
		cleanup()
		panic(err)
	}
	db = d
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func conceptStore(t *testing.T) *store.ConceptStore {
	t.Helper()
	s, err := store.Open(ctx, db.AppDSN, "okf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return store.NewConceptStore(s)
}

func newTools(t *testing.T) *Tools {
	t.Helper()
	return NewTools(conceptStore(t), uuid.New(), "mcp")
}

func admin(t *testing.T, sql string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, db.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatal(err)
	}
}

func connect(t *testing.T, tools *Tools) *mcp.ClientSession {
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

func seed(t *testing.T, tools *Tools, path string, kw map[string]any) {
	t.Helper()
	if _, ok := kw["type"]; !ok {
		kw["type"] = "Concept"
	}
	if _, err := tools.Create(ctx, path, kw); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, tools *Tools, path string) *concept {
	t.Helper()
	c, err := tools.Read(ctx, path)
	if err != nil || c == nil {
		t.Fatalf("read %s = %v, %v", path, c, err)
	}
	return c
}

// toolError asserts err is a ToolError and returns its message.
func toolError(t *testing.T, err error) string {
	t.Helper()
	var te *ToolError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want a *ToolError", err)
	}
	return te.Msg
}

func wantToolError(t *testing.T, err error, contains string) {
	t.Helper()
	if msg := toolError(t, err); !strings.Contains(msg, contains) {
		t.Fatalf("error %q does not contain %q", msg, contains)
	}
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	return res.Content[0].(*mcp.TextContent).Text
}

func TestCreateRejectsAConceptWithoutType(t *testing.T) {
	_, err := newTools(t).Create(ctx, "a/b", map[string]any{"type": "", "title": "x", "description": "", "body": "", "links": []any{}})
	wantToolError(t, err, "type is required")
}

// There is no delete tool, so one concept at `index` would make the whole tenant
// un-exportable for good.
func TestCreateRejectsAPathReservedForAGeneratedBundleFile(t *testing.T) {
	tools := newTools(t)
	_, err := tools.Create(ctx, "index", map[string]any{"type": "Concept"})
	wantToolError(t, err, "reserved")
	seed(t, tools, "architecture/index", map[string]any{})
}

func TestCreateRejectsAPathThatIsTaken(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"body": "first"})
	_, err := tools.Create(ctx, "a/b", map[string]any{"type": "Concept", "body": "second"})
	wantToolError(t, err, "already exists")
}

func TestLinksComeFromTheBodyNotFromTheArgument(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"body": "see [y](/a/y.md)", "links": []any{"forged/edge"}})
	if got := read(t, tools, "a/b").Links; !reflect.DeepEqual(got, []string{"a/y"}) {
		t.Fatalf("links = %v", got)
	}
}

func TestUpdateConflictReturnsCurrentContent(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/c", map[string]any{"title": "t", "body": "v1"})
	one := 1
	if _, err := tools.Update(ctx, "a/c", &one, map[string]any{"body": "v2"}); err != nil {
		t.Fatal(err)
	}
	got, err := tools.Update(ctx, "a/c", &one, map[string]any{"body": "v3"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (conflictResult{Conflict: true, CurrentVersion: 2, CurrentBody: "v2"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestUpdateKeepsTheFieldsItWasNotGiven(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"title": "Original", "description": "D", "body": "v1", "frontmatter": obj("owner", "sec")})
	if _, err := tools.Update(ctx, "a/b", nil, map[string]any{"body": "v2"}); err != nil {
		t.Fatal(err)
	}
	c := read(t, tools, "a/b")
	fm, _ := json.Marshal(c.Frontmatter)
	if c.Title != "Original" || c.Description != "D" || c.Body != "v2" || string(fm) != `{"owner":"sec"}` {
		t.Fatalf("got %+v %s", c, fm)
	}
}

func TestFrontmatterThatIsNotAnObjectIsCorrectable(t *testing.T) {
	_, err := newTools(t).Create(ctx, "a/b", map[string]any{"type": "Concept", "frontmatter": "owner: sec"})
	wantToolError(t, err, "frontmatter must be an object")
}

// null is the likeliest way an agent says "leave it alone"; it must not wipe.
func TestANullFrontmatterIsRefusedRatherThanErasing(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"body": "v1", "frontmatter": obj("owner", "sec")})
	_, err := tools.Update(ctx, "a/b", nil, map[string]any{"body": "v2", "frontmatter": nil})
	wantToolError(t, err, "frontmatter must be an object")
	fm, _ := json.Marshal(read(t, tools, "a/b").Frontmatter)
	if string(fm) != `{"owner":"sec"}` {
		t.Fatalf("frontmatter = %s", fm)
	}
}

func TestANullTextFieldIsRefusedRatherThanErasing(t *testing.T) {
	for _, field := range []string{"body", "type", "title", "description"} {
		t.Run(field, func(t *testing.T) {
			tools := newTools(t)
			seed(t, tools, "a/b", map[string]any{"title": "T", "description": "D", "body": "v1"})
			_, err := tools.Update(ctx, "a/b", nil, map[string]any{field: nil})
			wantToolError(t, err, field+" must be a string")
			c := read(t, tools, "a/b")
			if c.Body != "v1" || c.Type != "Concept" || c.Title != "T" || c.Description != "D" {
				t.Fatalf("got %+v", c)
			}
		})
	}
}

func TestUpdateOfAnotherTenantsPathIsIndistinguishableFromAbsent(t *testing.T) {
	tools := newTools(t)
	if _, _, err := tools.c.Create(ctx, uuid.New(), okf.Concept{Path: "a/b", Type: "Concept", Body: secret, Frontmatter: okf.NewMap()}, "seed"); err != nil {
		t.Fatal(err)
	}
	_, err := tools.Update(ctx, "a/b", nil, map[string]any{"body": "mine"})
	taken := toolError(t, err)
	_, err = tools.Update(ctx, "nowhere/at/all", nil, map[string]any{"body": "mine"})
	absent := toolError(t, err)
	if strings.Contains(taken, secret) {
		t.Fatalf("leaked a body: %q", taken)
	}
	if strings.ReplaceAll(taken, "a/b", "X") != strings.ReplaceAll(absent, "nowhere/at/all", "X") {
		t.Fatalf("distinguishable: %q vs %q", taken, absent)
	}
}

// Python monkeypatches read to return a phantom; here the phantom goes straight to write.
func TestUpdateOfARowThatVanishedMidWriteIsNotFound(t *testing.T) {
	tools := newTools(t)
	phantom := okf.Concept{Path: "ghost/path", Type: "Concept", Frontmatter: okf.NewMap()}
	_, err := tools.write(ctx, phantom, "ghost/path", nil, map[string]any{"body": "x"})
	if msg := toolError(t, err); msg != "no concept at ghost/path" {
		t.Fatalf("got %q", msg)
	}
}

func TestSearchReturnsCardsAndNeverABody(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "detect/dormant", map[string]any{"title": "Dormant Rule", "body": secret})
	hits, err := tools.Search(ctx, "dormant", 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "detect/dormant" {
		t.Fatalf("hits = %+v", hits)
	}
	if b, _ := json.Marshal(hits); strings.Contains(string(b), secret) {
		t.Fatalf("leaked a body: %s", b)
	}
}

func TestSearchTermsAreOredSoExtraTermsBroaden(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/one", map[string]any{"title": "Dormant Rule"})
	seed(t, tools, "a/two", map[string]any{"title": "Splunk Cursor"})
	hits, err := tools.Search(ctx, "dormant splunk", 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[h.Path] = true
	}
	if !reflect.DeepEqual(got, map[string]bool{"a/one": true, "a/two": true}) {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestGrepReturnsASnippetForEveryMatch(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"body": "alpha " + secret + " omega"})
	hits, err := tools.Grep(ctx, "al.ha", 5)
	if err != nil {
		t.Fatal(err)
	}
	if want := []grepHit{{Path: "a/b", Snippet: "alpha " + secret + " omega"}}; !reflect.DeepEqual(hits, want) {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestGrepReportsAnUnusablePatternWithoutABody(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"body": secret})
	_, err := tools.Grep(ctx, "alpha(", 5)
	if msg := toolError(t, err); strings.Contains(msg, secret) {
		t.Fatalf("leaked a body: %q", msg)
	}
}

func TestReadOfAMissingPathIsNil(t *testing.T) {
	c, err := newTools(t).Read(ctx, "nothing/here")
	if err != nil || c != nil {
		t.Fatalf("got %v, %v", c, err)
	}
}

func TestRelateRecordsAnEdgeBothWays(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/x", map[string]any{"body": "start"})
	seed(t, tools, "b/y", map[string]any{"body": "target"})
	if _, err := tools.Relate(ctx, "a/x", "b/y"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, tools, "a/x").Links; !reflect.DeepEqual(got, []string{"b/y"}) {
		t.Fatalf("links = %v", got)
	}
	if got := read(t, tools, "b/y").Backlinks; !reflect.DeepEqual(got, []string{"a/x"}) {
		t.Fatalf("backlinks = %v", got)
	}
}

// New: an agent that retries MUST NOT append the link twice.
func TestRelateIsIdempotent(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/x", map[string]any{"body": "start"})
	first, err := tools.Relate(ctx, "a/x", "b/y")
	if err != nil {
		t.Fatal(err)
	}
	second, err := tools.Relate(ctx, "a/x", "b/y")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || read(t, tools, "a/x").Body != "start\n\n[b/y](/b/y.md)\n" {
		t.Fatalf("first %+v, second %+v", first, second)
	}
}

func TestRelateRejectsAMissingSource(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "b/y", map[string]any{})
	_, err := tools.Relate(ctx, "a/x", "b/y")
	wantToolError(t, err, "no concept at a/x")
}

func TestListReturnsPathsAndCountsByType(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/one", map[string]any{})
	seed(t, tools, "a/two", map[string]any{"type": "Runbook"})
	seed(t, tools, "b/three", map[string]any{})
	got, err := tools.List(ctx, "a/")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(got); string(b) != `{"paths":["a/one","a/two"],"counts":{"Concept":1,"Runbook":1}}` {
		t.Fatalf("got %s", b)
	}
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

func TestTheServerAdvertisesExactlyTheSevenTools(t *testing.T) {
	if names := slices.Sorted(maps.Keys(listTools(t))); !reflect.DeepEqual(names, toolNames) {
		t.Fatalf("advertised %v", names)
	}
}

func TestSearchAndGrepAdvertiseLimitAsRequired(t *testing.T) {
	advertised := listTools(t)
	for _, name := range []string{"okf_search", "okf_grep"} {
		required := field(advertised[name].InputSchema, "required").([]any)
		if !slices.Contains(required, any("limit")) {
			t.Fatalf("%s required = %v", name, required)
		}
	}
}

func TestSearchAndGrepAdvertiseTheEnvelopeTheyAnswerIn(t *testing.T) {
	advertised := listTools(t)
	for _, name := range []string{"okf_search", "okf_grep"} {
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
	search := strings.ToLower(advertised["okf_search"].Description)
	for _, word := range []string{"lexical", "or-ed", "distinctive"} {
		if !strings.Contains(search, word) {
			t.Fatalf("okf_search description lacks %q", word)
		}
	}
}

func TestAToolCallRoundTripsOverTheProtocol(t *testing.T) {
	session := connect(t, newTools(t))
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "okf_create", Arguments: map[string]any{
		"path": "e2e/smoke", "type": "Concept", "title": "Smoke", "body": secret,
	}}); err != nil {
		t.Fatal(err)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "okf_search", Arguments: map[string]any{"query": "smoke", "limit": 5}})
	if err != nil {
		t.Fatal(err)
	}
	matched, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "okf_grep", Arguments: map[string]any{"pattern": "smoke", "limit": 5}})
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
	res, err := connect(t, newTools(t)).CallTool(ctx, &mcp.CallToolParams{Name: "okf_create", Arguments: map[string]any{"path": "a/b", "type": ""}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(text(t, res), "type is required") {
		t.Fatalf("got IsError=%v %q", res.IsError, text(t, res))
	}
}

func TestACallMissingARequiredArgumentIsAnErrorResult(t *testing.T) {
	res, err := connect(t, newTools(t)).CallTool(ctx, &mcp.CallToolParams{Name: "okf_search", Arguments: map[string]any{"query": "smoke"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("IsError = false")
	}
}

// A defect below the boundary must stay a defect: a tidy error result would send the
// agent to retry a request that was never wrong, and hide the bug.
func TestAnInternalErrorIsNotDressedUpAsTheAgentsMistake(t *testing.T) {
	session := connect(t, newTools(t))
	admin(t, `REVOKE SELECT ON okf.concept FROM okf_app`)
	t.Cleanup(func() { admin(t, `GRANT SELECT ON okf.concept TO okf_app`) })
	if res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "okf_list", Arguments: map[string]any{}}); err == nil {
		t.Fatalf("got a result, want a protocol error: %+v", res)
	}
}

// Plausible from an LLM, and a protocol error would give it nothing to act on.
func TestAnArgumentOfTheWrongShapeIsAnErrorResult(t *testing.T) {
	res, err := connect(t, newTools(t)).CallTool(ctx, &mcp.CallToolParams{Name: "okf_create", Arguments: map[string]any{
		"path": "a/b", "type": "Concept", "frontmatter": "owner: sec",
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Divergence 2: the wording is santhosh-tekuri/jsonschema's, but it still names
	// the field and the type it wanted.
	msg := text(t, res)
	if !res.IsError || !strings.Contains(msg, "frontmatter") || !strings.Contains(msg, "object") {
		t.Fatalf("got IsError=%v %q", res.IsError, msg)
	}
}

func TestSchemaErrorsAreSortedAndNameTheTool(t *testing.T) {
	res, err := connect(t, newTools(t)).CallTool(ctx, &mcp.CallToolParams{Name: "okf_search", Arguments: map[string]any{
		"query": 1, "limit": 201, "links": []any{},
	}})
	if err != nil {
		t.Fatal(err)
	}
	msg := text(t, res)
	if !res.IsError || !strings.HasPrefix(msg, "okf_search: $: ") ||
		strings.Index(msg, "; limit: ") > strings.Index(msg, "; query: ") {
		t.Fatalf("got IsError=%v %q", res.IsError, msg)
	}
}

// New: tools.py answers an unknown name with an error result, not a protocol error.
func TestAnUnknownToolIsAnErrorResult(t *testing.T) {
	res, err := connect(t, newTools(t)).CallTool(ctx, &mcp.CallToolParams{Name: "okf_delete", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || text(t, res) != "no such tool: okf_delete" {
		t.Fatalf("got IsError=%v %q", res.IsError, text(t, res))
	}
}

// New: the text content is json.dumps of the structured content, byte for byte.
func TestTextContentIsPythonJSONDumps(t *testing.T) {
	session := connect(t, newTools(t))
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "okf_create", Arguments: map[string]any{
		"path": "a/b", "type": "Concept", "body": "café \"<&>\"\n", "frontmatter": map[string]any{"n": 1},
	}}); err != nil {
		t.Fatal(err)
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "okf_read", Arguments: map[string]any{"path": "a/b"}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"path": "a/b", "type": "Concept", "title": "", "description": "", "body": "caf\u00e9 \"<&>\"\n", ` +
		`"frontmatter": {"n": 1}, "version": 1, "links": [], "backlinks": []}`
	if got := text(t, res); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "okf_read", Arguments: map[string]any{"path": "no/such"}})
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

// What go-sdk puts on the wire, not what toolDefinitions returns: the SDK sorted
// the tools by name and added cacheScope "public", which Python never sends.
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
	want := []string{"okf_list", "okf_search", "okf_grep", "okf_read", "okf_create", "okf_update", "okf_relate"}
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
	// NOLOGIN makes the reconnect fail too, which is what a database that is down looks like.
	admin(t, `ALTER ROLE okf_app NOLOGIN`)
	t.Cleanup(func() { admin(t, `ALTER ROLE okf_app LOGIN`) })
	admin(t, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = 'okf_app'`)

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "okf_list", Arguments: map[string]any{}})
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

// pythonWire is testdata/python_wire.json: requests and the result the Python server
// (2de90d2) answered them with, on an empty tenant.
type pythonWire struct {
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
	var cases []pythonWire
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
