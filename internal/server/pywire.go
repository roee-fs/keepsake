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
	"tools/list": func(m *okf.Map, modern bool) {
		if !modern {
			return
		}
		m.Set("cacheScope", "private")
		m.Set("resultType", "complete")
		m.Set("ttlMs", 0)
		tools, _ := m.Get("tools")
		for _, t := range tools.([]any) {
			tool := t.(*okf.Map)
			// Modern tools carry each schema as declared; only the legacy model reorders it.
			for _, k := range []string{"inputSchema", "outputSchema"} {
				if s, ok := tool.Get(k); ok {
					tool.Set(k, first(s.(*okf.Map), "type", "properties", "required"))
				}
			}
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

// pythonResults rewrites go-sdk's successful JSON-RPC results into Python's shapes for
// the request's era. Errors, notifications and anything unparsed pass through unchanged.
func pythonResults(next http.Handler) http.Handler {
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
		var call struct{ Method string }
		json.Unmarshal(body, &call)
		shape := shapes[call.Method]
		if shape == nil {
			next.ServeHTTP(w, r)
			return
		}
		buf := &buffered{header: w.Header()}
		next.ServeHTTP(buf, r)
		out := buf.body.Bytes()
		var msg okf.Map
		if buf.code == http.StatusOK && json.Unmarshal(out, &msg) == nil {
			if result, ok := msg.Get("result"); ok {
				if m, ok := result.(*okf.Map); ok {
					shape(m, modernEra(r))
					msg.Set("result", pyModel(m))
					if b, err := json.Marshal(&msg); err == nil {
						out = b
					}
				}
			}
		}
		w.Header().Del("Content-Length")
		if buf.code != 0 {
			w.WriteHeader(buf.code)
		}
		w.Write(out)
	})
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
