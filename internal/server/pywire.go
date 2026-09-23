package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/roee-fs/keepsake/okf"
)

// modernEra is Python's era routing: an mcp-protocol-version header that is not a
// handshake version goes to the modern transport.
func modernEra(r *http.Request) bool {
	v := r.Header.Values("Mcp-Protocol-Version")
	return len(v) > 0 && !slices.Contains(handshakeVersions, v[0])
}

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

// pyModel orders a model's keys as mcp_types declares its fields: alphabetically,
// with _meta last. Only models are reordered, never the data they carry.
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

// pythonMethods are the requests Python's server answers in each era. go-sdk
// answers more (resources/list, logging/setLevel, ...); Python says -32601.
var pythonMethods = map[bool][]string{
	false: {"initialize", "ping", "tools/list", "tools/call"},
	true:  {"server/discover", "tools/list", "tools/call"},
}

const (
	codeParse         = -32700
	codeInvalidReq    = -32600
	codeNotFound      = -32601
	codeInvalidParams = -32602
	codeHeader        = -32020
	codeVersion       = -32022
)

// rpcError answers a JSON-RPC error. Divergence 11: status and code are Python's,
// the message may be worded differently.
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

// envelopeLadder is Python's modern ladder for a request whose mcp-protocol-version header
// names no known version: it returns the status, code and message, or 0.
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

// badParams is the params validation Python runs before dispatch, for the requests
// whose params go-sdk would accept or check later.
func badParams(method string, params *okf.Map, r *http.Request) bool {
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
		if modernEra(r) && (r.Header.Get("Mcp-Method") != method || hasName && r.Header.Get("Mcp-Name") != name) {
			return false // go-sdk's header checks answer -32020, as Python's do.
		}
		_, nameString := name.(string)
		return !hasName || !nameString || hasArgs && args != nil && !argsObj
	case "initialize":
		if modernEra(r) {
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

// pythonWire answers what Python's transports reject before dispatch with Python's
// status and code, and rewrites go-sdk's successful results into Python's shapes.
func pythonWire(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		modern := modernEra(r)
		if !json.Valid(body) {
			rpcError(w, http.StatusBadRequest, nil, codeParse, "Parse error")
			return
		}
		var msg okf.Map
		if bytes.TrimSpace(body)[0] != '{' || json.Unmarshal(body, &msg) != nil || !validMessage(&msg) {
			if modern {
				rpcError(w, http.StatusBadRequest, nil, codeInvalidReq, "Body must be a single JSON-RPC request or notification object")
			} else {
				rpcError(w, http.StatusBadRequest, nil, codeInvalidParams, "Validation error")
			}
			return
		}
		m, _ := msg.Get("method")
		method, _ := m.(string)
		id, hasID := msg.Get("id")
		p, _ := msg.Get("params")
		params, _ := p.(*okf.Map)
		isRequest := method != "" && hasID && validID(id)
		if method != "" && !isRequest {
			if !modern {
				// Python forwards a notification unanswered; pydantic reads a request
				// whose id is not a string or integer as one.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if v := r.Header.Values("Mcp-Protocol-Version"); isRequest && len(v) > 0 && v[0] != "2026-07-28" && !slices.Contains(handshakeVersions, v[0]) {
			if params == nil {
				params = okf.NewMap()
			}
			code, text := envelopeLadder(params)
			rpcError(w, http.StatusBadRequest, id, code, text)
			return
		}
		if isRequest && badParams(method, params, r) {
			status := http.StatusOK
			if modern {
				status = http.StatusBadRequest
			}
			rpcError(w, status, id, codeInvalidParams, "Invalid request parameters")
			return
		}
		buf := &buffered{header: w.Header()}
		next.ServeHTTP(buf, r)
		out := buf.body.Bytes()
		code := buf.code
		var res okf.Map
		switch {
		case isRequest && code == http.StatusBadRequest && bytes.HasPrefix(out, []byte("JSON RPC not handled")),
			isRequest && code == http.StatusOK && json.Unmarshal(out, &res) == nil && has(&res, "result") && !slices.Contains(pythonMethods[modern], method):
			status := http.StatusOK
			if modern {
				status = http.StatusNotFound
			}
			rpcError(w, status, id, codeNotFound, "Method not found", method)
			return
		case code == http.StatusOK && shapes[method] != nil && json.Unmarshal(out, &res) == nil:
			if result, ok := res.Get("result"); ok {
				if m, ok := result.(*okf.Map); ok {
					shapes[method](m, modern)
					res.Set("result", pyModel(m))
					if b, err := json.Marshal(&res); err == nil {
						out = b
					}
				}
			}
		}
		w.Header().Del("Content-Length")
		if code != 0 {
			w.WriteHeader(code)
		}
		w.Write(out)
	})
}

func has(m *okf.Map, k string) bool {
	_, ok := m.Get(k)
	return ok
}

// buffered holds a response so its result can be rewritten before it is sent.
type buffered struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (b *buffered) Header() http.Header { return b.header }

func (b *buffered) WriteHeader(code int) {
	if b.code == 0 {
		b.code = code
	}
}

func (b *buffered) Write(p []byte) (int, error) {
	b.WriteHeader(http.StatusOK)
	return b.body.Write(p)
}

func (b *buffered) Flush() {}
