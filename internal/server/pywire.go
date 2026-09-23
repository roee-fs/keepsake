package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
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

// rpcError answers a JSON-RPC error with Python's status and code; divergence 11 is the wording.
func rpcError(w http.ResponseWriter, status int, id any, code int, msg string, data ...any) {
	e := obj("code", code, "message", msg)
	if len(data) > 0 {
		e.Set("data", data[0])
	}
	b, _ := json.Marshal(obj("jsonrpc", "2.0", "id", id, "error", e))
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

// envelopeLadder is Python's answer to a request naming an unknown protocol version.
func envelopeLadder(params *okf.Map) (int, string) {
	meta, _ := params.Get("_meta")
	m, ok := meta.(*okf.Map)
	if !ok {
		return codeInvalidParams, "params._meta must be an object carrying the required envelope keys"
	}
	pv, hasPV := m.Get("io.modelcontextprotocol/protocolVersion")
	if _, hasCaps := m.Get("io.modelcontextprotocol/clientCapabilities"); !hasPV || !hasCaps {
		return codeInvalidParams, "params._meta is missing the required envelope key(s)"
	}
	if s, ok := pv.(string); !ok {
		return codeInvalidParams, "the protocol-version envelope value must be a string"
	} else if s != "2026-07-28" {
		return codeVersion, "Unsupported protocol version"
	}
	return codeHeader, "mcp-protocol-version header does not match the request envelope's protocol version"
}

// badParams is Python's pre-dispatch params check, for requests go-sdk would accept or check later.
func badParams(method string, params *okf.Map, r *http.Request, e era) bool {
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
		if e != legacy && (r.Header.Get("Mcp-Method") != method || hasName && r.Header.Get("Mcp-Name") != name) {
			return false // go-sdk's header checks answer -32020, as Python's do.
		}
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
	var syntax *json.SyntaxError
	switch {
	case errors.As(err, &syntax):
		rpcError(w, http.StatusBadRequest, nil, codeParse, "Parse error")
		return true
	case err != nil || !validMessage(&msg):
		if e != legacy {
			rpcError(w, http.StatusBadRequest, nil, codeInvalidReq, "Body must be a single JSON-RPC request or notification object")
		} else {
			rpcError(w, http.StatusBadRequest, nil, codeInvalidParams, "Validation error")
		}
		return true
	}
	m, _ := msg.Get("method")
	method, _ := m.(string)
	id, hasID := msg.Get("id")
	p, _ := msg.Get("params")
	params, _ := p.(*okf.Map)
	isRequest := method != "" && hasID && validID(id)
	switch {
	case method != "" && !isRequest:
		if e != legacy {
			return false
		}
		// Python forwards a notification unanswered, and reads a request with a non-integer id as one.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
	case isRequest && e == unknown:
		if params == nil {
			params = okf.NewMap()
		}
		code, text := envelopeLadder(params)
		rpcError(w, http.StatusBadRequest, id, code, text)
	case isRequest && badParams(method, params, r, e):
		status := http.StatusOK
		if e != legacy {
			status = http.StatusBadRequest
		}
		rpcError(w, status, id, codeInvalidParams, "Invalid request parameters")
	case isRequest && e == legacy && !pythonServes(method, e):
		// The modern transport checks its headers first, so go-sdk answers there.
		rpcError(w, http.StatusOK, id, codeNotFound, "Method not found", method)
	default:
		return false
	}
	return true
}

// methodNotFound is -32601 as Python words it; go-sdk sends it as 404 to a modern request.
func methodNotFound(method string) error {
	data, _ := json.Marshal(method)
	return &jsonrpc.Error{Code: codeNotFound, Message: "Method not found", Data: data}
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
