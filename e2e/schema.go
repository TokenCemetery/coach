package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Validate checks a JSON value decoded with json.Decoder.UseNumber against an
// OpenAPI 3.0 schema. It supports the keywords api/openapi.yaml uses: $ref,
// type, nullable, enum, properties, additionalProperties and items. The
// contract declares no required properties, so missing fields are accepted.
func (c *Contract) Validate(v any, schema map[string]any) []string {
	var problems []string
	c.validate(v, schema, "$", &problems)
	return problems
}

func (c *Contract) validate(v any, schema map[string]any, at string, problems *[]string) {
	for {
		ref, ok := schema["$ref"].(string)
		if !ok {
			break
		}
		resolved, err := c.resolve(ref)
		if err != nil {
			*problems = append(*problems, fmt.Sprintf("%s: %v", at, err))
			return
		}
		schema = resolved
	}
	if v == nil {
		if schema["nullable"] != true && schema["type"] != nil {
			*problems = append(*problems, fmt.Sprintf("%s: null, want %v", at, schema["type"]))
		}
		return
	}
	kind := str(schema["type"])
	if kind == "" && schema["properties"] != nil {
		kind = "object"
	}
	fail := func(want string) {
		*problems = append(*problems, fmt.Sprintf("%s: %s, want %s", at, describe(v), want))
	}
	switch kind {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			fail("object")
			return
		}
		props := mapOf(schema["properties"])
		extra, _ := schema["additionalProperties"].(map[string]any)
		for k, x := range obj {
			if s, ok := props[k]; ok {
				c.validate(x, mapOf(s), at+"."+k, problems)
			} else if extra != nil {
				c.validate(x, extra, at+"."+k, problems)
			}
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			fail("array")
			return
		}
		items := mapOf(schema["items"])
		for i, x := range arr {
			c.validate(x, items, fmt.Sprintf("%s[%d]", at, i), problems)
		}
	case "string":
		s, ok := v.(string)
		if !ok {
			fail("string")
			return
		}
		if enum := list(schema["enum"]); len(enum) > 0 && !contains(enum, s) {
			*problems = append(*problems, fmt.Sprintf("%s: %q not in enum %v", at, s, enum))
		}
	case "integer":
		n, ok := v.(json.Number)
		if !ok || strings.ContainsAny(n.String(), ".eE") {
			fail("integer")
		}
	case "number":
		if _, ok := v.(json.Number); !ok {
			fail("number")
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			fail("boolean")
		}
	}
}

func contains(enum []any, s string) bool {
	for _, e := range enum {
		if e == s {
			return true
		}
	}
	return false
}

func describe(v any) string {
	switch v := v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case json.Number:
		return "number " + v.String()
	case bool:
		return "boolean"
	}
	return fmt.Sprintf("%T", v)
}
