package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/roee-fs/keepsake/okf"
)

// era is the Python transport a request reaches, from its mcp-protocol-version header.
type era int

const (
	// legacy is no header or a handshake version.
	legacy era = iota
	// modern is the one version the modern transport serves.
	modern
	// unknown is any other version, which the modern transport's envelope ladder refuses.
	unknown
)

// handshakeVersions route a request to Python's legacy transport.
var handshakeVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"}

func eraOf(r *http.Request) era {
	v := r.Header.Values("Mcp-Protocol-Version")
	switch {
	case len(v) == 0 || slices.Contains(handshakeVersions, v[0]):
		return legacy
	case v[0] == "2026-07-28":
		return modern
	}
	return unknown
}

type eraKey struct{}

// shapes rewrite a successful result into what Python's mcp_types models serialise.
var shapes = map[string]func(result *okf.Map, modern bool){
	"initialize": func(m *okf.Map, _ bool) {
		m.Set("capabilities", obj("experimental", okf.NewMap(), "tools", obj("listChanged", false)))
	},
	"server/discover": func(m *okf.Map, _ bool) {
		m.Set("cacheScope", "private")
		m.Set("capabilities", obj("tools", obj("listChanged", false)))
		m.Set("resultType", "complete")
		m.Set("supportedVersions", []string{"2026-07-28"})
		m.Set("ttlMs", 0)
	},
	"tools/list": func(m *okf.Map, modern bool) {
		if !modern {
			return
		}
		m.Set("cacheScope", "private")
		m.Set("resultType", "complete")
		m.Set("ttlMs", 0)
		tools, _ := m.Get("tools")
		for i, t := range tools.([]any) {
			tool := pyModel(t.(*okf.Map))
			tools.([]any)[i] = tool
			// Modern tools carry each schema as declared; only the legacy model reorders it.
			for _, k := range []string{"inputSchema", "outputSchema"} {
				if s, ok := tool.Get(k); ok {
					tool.Set(k, first(s.(*okf.Map), "type", "properties", "required"))
				}
			}
		}
	},
	"tools/call": func(m *okf.Map, modern bool) {
		if _, ok := m.Get("isError"); !ok {
			m.Set("isError", false)
		}
		if modern {
			m.Set("resultType", "complete")
		}
		content, _ := m.Get("content")
		for i, c := range content.([]any) {
			content.([]any)[i] = pyModel(c.(*okf.Map))
		}
	},
}

// pyModel orders a model's keys as mcp_types does: alphabetically, _meta last; data keeps its order.
func pyModel(m *okf.Map) *okf.Map {
	keys := m.Keys()
	slices.SortFunc(keys, func(a, b string) int {
		if (a == "_meta") != (b == "_meta") {
			if a == "_meta" {
				return 1
			}
			return -1
		}
		return strings.Compare(a, b)
	})
	return first(m, keys...)
}

// first returns m with keys moved to the front, in that order.
func first(m *okf.Map, keys ...string) *okf.Map {
	out := okf.NewMap()
	for _, k := range append(keys, m.Keys()...) {
		if _, done := out.Get(k); done {
			continue
		}
		if v, ok := m.Get(k); ok {
			out.Set(k, v)
		}
	}
	return out
}

// pythonMethods are the requests Python answers per era; go-sdk answers more, where Python says -32601.
var pythonMethods = map[bool][]string{
	false: {"initialize", "ping", "tools/list", "tools/call"},
	true:  {"server/discover", "tools/list", "tools/call"},
}

// pythonServes reports whether Python answers method in era e.
func pythonServes(method string, e era) bool {
	return slices.Contains(pythonMethods[e != legacy], method)
}

const (
	codeParse         = -32700
	codeInvalidReq    = -32600
	codeNotFound      = -32601
	codeInvalidParams = -32602
	codeHeader        = -32020
	codeVersion       = -32022
)

// rpcError answers a JSON-RPC error with Python's status and code. The modern transport
// writes a null id last; legacy messages keep divergence 11's wording.
func rpcError(w http.ResponseWriter, e era, status int, id any, code int, msg string, data ...any) {
	errObj := obj("code", code, "message", msg)
	if len(data) > 0 {
		errObj.Set("data", data[0])
	}
	body := obj("jsonrpc", "2.0", "id", id, "error", errObj)
	if e != legacy && id == nil {
		body = obj("jsonrpc", "2.0", "error", errObj, "id", nil)
	}
	b, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

// validID is a JSON-RPC id pydantic accepts: a string or an integral number.
func validID(v any) bool {
	switch v := v.(type) {
	case string:
		return true
	case json.Number:
		f, err := v.Float64()
		return err == nil && f == float64(int64(f))
	}
	return false
}

// validMessage is jsonrpc_message_adapter: a request, a notification, a response or an error.
func validMessage(m *okf.Map) bool {
	if v, _ := m.Get("jsonrpc"); v != "2.0" {
		return false
	}
	if method, ok := m.Get("method"); ok {
		params, has := m.Get("params")
		_, isObj := params.(*okf.Map)
		_, isString := method.(string)
		return isString && (!has || params == nil || isObj)
	}
	id, _ := m.Get("id")
	result, hasResult := m.Get("result")
	errObj, hasError := m.Get("error")
	_, resultObj := result.(*okf.Map)
	_, errorObj := errObj.(*okf.Map)
	return (validID(id) || id == nil) && (hasResult && resultObj || hasError && errorObj)
}

const (
	versionKey = "io.modelcontextprotocol/protocolVersion"
	capsKey    = "io.modelcontextprotocol/clientCapabilities"
)

// nameBearing maps a method to the params key its Mcp-Name header mirrors.
var nameBearing = map[string]string{"tools/call": "name", "prompts/get": "name", "resources/read": "uri"}

var base64Sentinel = regexp.MustCompile(`^=\?base64\?(.*)\?=$`)

// headerValue is decode_header_value: the value, or its canonical base64 payload as UTF-8.
func headerValue(v string) (string, bool) {
	m := base64Sentinel.FindStringSubmatch(v)
	if m == nil {
		return v, true
	}
	b, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil || base64.StdEncoding.EncodeToString(b) != m[1] || !utf8.Valid(b) {
		return "", false
	}
	return string(b), true
}

// unsupportedVersion is Python's -32022 for a protocol version the modern transport does not serve.
func unsupportedVersion(w http.ResponseWriter, e era, id, requested any) {
	rpcError(w, e, http.StatusBadRequest, id, codeVersion, "Unsupported protocol version",
		obj("supported", []string{"2026-07-28"}, "requested", requested))
}

// ladder is classify_inbound_request: the first envelope or header rung a modern request fails.
func ladder(r *http.Request, method string, params *okf.Map) (code int, msg string) {
	var meta *okf.Map
	if params != nil {
		v, _ := params.Get("_meta")
		meta, _ = v.(*okf.Map)
	}
	if meta == nil {
		return codeInvalidParams, "params._meta must be an object carrying the required '" + versionKey + "' and '" + capsKey + "' envelope keys"
	}
	var missing []string
	for _, k := range []string{versionKey, capsKey} {
		if _, ok := meta.Get(k); !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return codeInvalidParams, "params._meta is missing the required envelope key(s): " + strings.Join(missing, ", ")
	}
	pv, _ := meta.Get(versionKey)
	if v := r.Header.Values("Mcp-Protocol-Version"); len(v) == 0 || pv != any(v[0]) {
		return codeHeader, "mcp-protocol-version header does not match the request envelope's protocol version"
	}
	if v := r.Header.Values("Mcp-Method"); len(v) == 0 || v[0] != method {
		return codeHeader, "mcp-method header does not match the request body's method"
	}
	if key := nameBearing[method]; key != "" {
		if body, _ := params.Get(key); body != nil {
			v := r.Header.Values("Mcp-Name")
			header, ok := "", false
			if len(v) > 0 {
				header, ok = headerValue(v[0])
			}
			if s, isString := body.(string); !ok || !isString || s != header {
				return codeHeader, "mcp-name header does not match the request body's '" + key + "' parameter"
			}
		}
	}
	// A non-string version already failed the header rung, which is always present here.
	return 0, ""
}

// badParams is Python's pre-dispatch params check, for requests go-sdk would accept or check later.
func badParams(method string, params *okf.Map, e era) bool {
	get := func(k string) (any, bool) {
		if params == nil {
			return nil, false
		}
		return params.Get(k)
	}
	switch method {
	case "tools/call":
		name, hasName := get("name")
		args, hasArgs := get("arguments")
		_, argsObj := args.(*okf.Map)
		_, nameString := name.(string)
		return !hasName || !nameString || hasArgs && args != nil && !argsObj
	case "initialize":
		if e != legacy {
			return false
		}
		pv, _ := get("protocolVersion")
		caps, _ := get("capabilities")
		info, _ := get("clientInfo")
		_, pvString := pv.(string)
		_, capsObj := caps.(*okf.Map)
		i, infoObj := info.(*okf.Map)
		if !pvString || !capsObj || !infoObj {
			return true
		}
		name, _ := i.Get("name")
		version, _ := i.Get("version")
		_, n := name.(string)
		_, v := version.(string)
		return !n || !v
	}
	return false
}

// rejected answers what Python refuses before dispatch, as Python does, and reports whether it did.
func rejected(w http.ResponseWriter, r *http.Request, e era) bool {
	// limitBody has already buffered it.
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	var msg okf.Map
	err := json.Unmarshal(body, &msg)
	if syntax := (*json.SyntaxError)(nil); errors.As(err, &syntax) {
		rpcError(w, e, http.StatusBadRequest, nil, codeParse, "Parse error")
		return true
	}
	if e != legacy {
		return rejectedModern(w, r, e, &msg, err == nil)
	}
	if err != nil || !validMessage(&msg) {
		rpcError(w, e, http.StatusBadRequest, nil, codeInvalidParams, "Validation error")
		return true
	}
	m, _ := msg.Get("method")
	method, _ := m.(string)
	id, hasID := msg.Get("id")
	p, _ := msg.Get("params")
	params, _ := p.(*okf.Map)
	switch {
	case method != "" && !(hasID && validID(id)):
		// Python forwards a notification unanswered, and reads a request with a non-integer id as one.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
	case method != "" && badParams(method, params, e):
		rpcError(w, e, http.StatusOK, id, codeInvalidParams, "Invalid request parameters", "")
	case method != "" && !pythonServes(method, e):
		rpcError(w, e, http.StatusOK, id, codeNotFound, "Method not found", method)
	default:
		return false
	}
	return true
}

// rejectedModern is handle_modern_request's pre-dispatch half, in its order.
func rejectedModern(w http.ResponseWriter, r *http.Request, e era, msg *okf.Map, isObject bool) bool {
	m, _ := msg.Get("method")
	method, isString := m.(string)
	p, hasParams := msg.Get("params")
	params, isObj := p.(*okf.Map)
	id, hasID := msg.Get("id")
	jsonrpc, _ := msg.Get("jsonrpc")
	if !isObject || jsonrpc != "2.0" || !isString || hasParams && p != nil && !isObj || hasID && !modernID(id) {
		rpcError(w, e, http.StatusBadRequest, nil, codeInvalidReq, "Body must be a single JSON-RPC request or notification object")
		return true
	}
	if !hasID {
		// A notification is acknowledged and dropped, at a version this transport serves.
		if e == unknown {
			unsupportedVersion(w, e, nil, r.Header.Get("Mcp-Protocol-Version"))
		} else {
			w.WriteHeader(http.StatusAccepted)
		}
		return true
	}
	for _, h := range []string{"mcp-protocol-version", "mcp-method", "mcp-name"} {
		if len(r.Header.Values(h)) > 1 {
			rpcError(w, e, http.StatusBadRequest, id, codeHeader, h+" header appears more than once")
			return true
		}
	}
	if code, text := ladder(r, method, params); code != 0 {
		rpcError(w, e, http.StatusBadRequest, id, code, text)
		return true
	}
	switch {
	case e == unknown:
		// The ladder proved the header equals the envelope's version.
		unsupportedVersion(w, e, id, r.Header.Get("Mcp-Protocol-Version"))
	case !pythonServes(method, e):
		rpcError(w, e, http.StatusNotFound, id, codeNotFound, "Method not found", method)
	case badParams(method, params, e):
		rpcError(w, e, http.StatusBadRequest, id, codeInvalidParams, "Invalid request parameters", "")
	default:
		return false
	}
	return true
}

// modernID is a RequestId pydantic accepts from JSON: a string or an integer literal.
func modernID(v any) bool {
	switch v := v.(type) {
	case string:
		return true
	case json.Number:
		return !strings.ContainsAny(string(v), ".eE")
	}
	return false
}

// shaped is a result in Python's key order, with the _meta go-sdk adds after the middleware last.
type shaped struct {
	mcp.ResultBase
	m *okf.Map
}

func (r *shaped) MarshalJSON() ([]byte, error) {
	meta := r.GetMeta()
	if len(meta) == 0 {
		return json.Marshal(r.m)
	}
	m := first(r.m)
	m.Set("_meta", meta)
	return json.Marshal(m)
}

// shape reorders res as Python's mcp_types model for method serialises it.
func shape(method string, e era, res mcp.Result) (*okf.Map, error) {
	b, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	m := okf.NewMap()
	if err := json.Unmarshal(b, m); err != nil {
		return nil, err
	}
	shapes[method](m, e != legacy)
	return pyModel(m), nil
}
