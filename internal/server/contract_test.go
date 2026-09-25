package server

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/okf"
)

// contractQuery is the query each templated route is exercised with.
func contractQuery(route string, tenant uuid.UUID) url.Values {
	q := url.Values{"tenant": {tenant.String()}}
	switch route {
	case "/search":
		q.Set("q", "dormant")
	case "/grep":
		q.Set("pattern", "Dormant")
	}
	return q
}

// TestResponsesMatchTheContract checks every route's response against
// frontend/openapi.json, which the generated TypeScript client is built from.
func TestResponsesMatchTheContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("../../frontend/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(ctx); err != nil {
		t.Fatal(err)
	}

	c := newConsole(t).login()
	tenant := c.seeded()
	fm := okf.NewMap()
	fm.Set("tags", []any{"x", 1})
	if _, _, err := c.cs.Create(ctx, tenant, okf.Concept{
		Path: "detect/linked", Type: "Rule", Title: "Linked", Body: "see [[detect/dormant]] and [[ghost]]",
		Frontmatter: fm, Links: []string{"detect/dormant", "ghost"},
	}, "test"); err != nil {
		t.Fatal(err)
	}

	for _, rt := range routeTable {
		method, pattern, _ := strings.Cut(rt.pattern, " ")
		specPath := strings.Replace(pattern, "{path...}", "{path}", 1)
		item := doc.Paths.Find(specPath)
		if item == nil || item.GetOperation(method) == nil {
			t.Errorf("%s is not in the contract", rt.pattern)
			continue
		}
		target := strings.Replace(pattern, "{path...}", "detect/linked", 1)
		var body io.Reader
		if rt.pattern == "POST /session" {
			body = strings.NewReader(`{"password": "` + adminPassword + `"}`)
		} else if method == http.MethodGet {
			target += "?" + contractQuery(pattern, tenant).Encode()
		}
		req, _ := http.NewRequest(method, target, body)
		req.Header.Set("Content-Type", "application/json")
		rec := c.do(req)
		if rec.Code >= 300 {
			t.Errorf("%s = %d %s", rt.pattern, rec.Code, rec.Body)
			continue
		}
		err := openapi3filter.ValidateResponse(ctx, &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{
				Request:    req,
				PathParams: map[string]string{"path": "detect/linked"},
				Route:      &routers.Route{Spec: doc, Path: specPath, PathItem: item, Method: method, Operation: item.GetOperation(method)},
			},
			Status:  rec.Code,
			Header:  rec.Header(),
			Body:    io.NopCloser(bytes.NewReader(rec.Body.Bytes())),
			Options: &openapi3filter.Options{IncludeResponseStatus: true},
		})
		if err != nil {
			t.Errorf("%s: %v", rt.pattern, err)
		}
	}

	// Every path in the contract MUST be served.
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			want := method + " " + strings.Replace(path, "{path}", "{path...}", 1)
			found := false
			for _, rt := range routeTable {
				found = found || rt.pattern == want
			}
			if !found {
				t.Errorf("the contract has %s but no route serves it", want)
			}
		}
	}
}
