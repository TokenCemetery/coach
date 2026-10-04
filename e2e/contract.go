// Package e2e checks a running coach binary against api/openapi.yaml.
package e2e

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Contract is the subset of api/openapi.yaml the tests use.
type Contract struct {
	Schemas    map[string]any
	Operations []Operation
	components map[string]any
}

// Operation is one method on one path of the contract.
type Operation struct {
	ID      string
	Method  string
	Path    string
	Summary string
	Tag     string
	Params  []Param
	Body    *Body
	Secured bool
	// Response is the JSON schema of the 200 response, or nil when the
	// contract documents no body.
	Response map[string]any
}

// Param is a path, query, or header parameter.
type Param struct {
	Name     string
	In       string
	Required bool
	Schema   map[string]any
}

// Body is the request body of an operation.
type Body struct {
	ContentType string
	Schema      map[string]any
}

// Mutating reports whether the operation may change server state, in which
// case it runs against its own server instance.
func (op Operation) Mutating() bool {
	return op.Method != http.MethodGet && op.Method != http.MethodHead
}

var methods = []string{"get", "head", "post", "put", "delete"}

// LoadContract reads an OpenAPI 3 document.
func LoadContract(path string) (*Contract, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is the repository contract
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	components := mapOf(doc["components"])
	c := &Contract{Schemas: mapOf(components["schemas"]), components: components}
	paths := mapOf(doc["paths"])
	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, p := range keys {
		item := mapOf(paths[p])
		for _, m := range methods {
			raw, ok := item[m]
			if !ok {
				continue
			}
			op, err := c.operation(p, m, mapOf(raw))
			if err != nil {
				return nil, err
			}
			c.Operations = append(c.Operations, op)
		}
	}
	return c, nil
}

func (c *Contract) operation(path, method string, o map[string]any) (Operation, error) {
	op := Operation{
		ID:      str(o["operationId"]),
		Method:  strings.ToUpper(method),
		Path:    path,
		Summary: str(o["summary"]),
		Secured: len(list(o["security"])) > 0,
	}
	if op.ID == "" {
		return op, fmt.Errorf("%s %s: missing operationId", op.Method, path)
	}
	if tags := list(o["tags"]); len(tags) > 0 {
		op.Tag = str(tags[0])
	}
	for _, raw := range list(o["parameters"]) {
		p := mapOf(raw)
		op.Params = append(op.Params, Param{
			Name:     str(p["name"]),
			In:       str(p["in"]),
			Required: p["required"] == true,
			Schema:   mapOf(p["schema"]),
		})
	}
	if rb, ok := o["requestBody"]; ok {
		for ct, media := range mapOf(mapOf(rb)["content"]) {
			op.Body = &Body{ContentType: ct, Schema: mapOf(mapOf(media)["schema"])}
			break
		}
	}
	ok := mapOf(mapOf(o["responses"])["200"])
	if schema, found := mapOf(mapOf(ok["content"])["application/json"])["schema"]; found {
		op.Response = mapOf(schema)
	}
	if err := c.checkRefs(o); err != nil {
		return op, fmt.Errorf("%s: %w", op.ID, err)
	}
	return op, nil
}

// checkRefs fails on a $ref that does not resolve, so a broken contract is
// reported once at load time instead of as a failure of every test using it.
func (c *Contract) checkRefs(v any) error {
	switch v := v.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			kind, name, _ := strings.Cut(strings.TrimPrefix(ref, "#/components/"), "/")
			if _, found := mapOf(c.components[kind])[name]; !found {
				return fmt.Errorf("unresolved $ref %q", ref)
			}
		}
		for _, x := range v {
			if err := c.checkRefs(x); err != nil {
				return err
			}
		}
	case []any:
		for _, x := range v {
			if err := c.checkRefs(x); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Contract) resolve(ref string) (map[string]any, error) {
	const prefix = "#/components/schemas/"
	if !strings.HasPrefix(ref, prefix) {
		return nil, fmt.Errorf("unsupported $ref %q", ref)
	}
	s, ok := c.Schemas[strings.TrimPrefix(ref, prefix)]
	if !ok {
		return nil, fmt.Errorf("unresolved $ref %q", ref)
	}
	return mapOf(s), nil
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
