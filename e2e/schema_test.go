package e2e

import (
	"encoding/json"
	"strings"
	"testing"
)

func decode(t *testing.T, s string) any {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestValidate(t *testing.T) {
	c := &Contract{Schemas: map[string]any{
		"Item": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"Name":  map[string]any{"type": "string"},
				"Ticks": map[string]any{"type": "integer", "format": "int64"},
				"Kind":  map[string]any{"type": "string", "enum": []any{"Movie", "Episode"}},
				"Note":  map[string]any{"type": "string", "nullable": true},
				"Tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"Ids":   map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
			},
		},
	}}
	schema := map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Item"}}
	cases := []struct {
		name, body string
		problems   []string
	}{
		{"valid", `[{"Name":"a","Ticks":10,"Kind":"Movie","Note":null,"Tags":["x"],"Ids":{"Tmdb":"1"},"Unknown":1}]`, nil},
		{"wrong container", `{}`, []string{"$: object, want array"}},
		{"wrong scalar", `[{"Name":1}]`, []string{"$[0].Name: number 1, want string"}},
		{"fractional integer", `[{"Ticks":1.5}]`, []string{"$[0].Ticks: number 1.5, want integer"}},
		{"enum", `[{"Kind":"Song"}]`, []string{`$[0].Kind: "Song" not in enum [Movie Episode]`}},
		{"null not nullable", `[{"Name":null}]`, []string{"$[0].Name: null, want string"}},
		{"array item", `[{"Tags":[true]}]`, []string{"$[0].Tags[0]: boolean, want string"}},
		{"additional property", `[{"Ids":{"Tmdb":2}}]`, []string{"$[0].Ids.Tmdb: number 2, want string"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.Validate(decode(t, tc.body), schema)
			if strings.Join(got, "\n") != strings.Join(tc.problems, "\n") {
				t.Fatalf("problems = %q, want %q", got, tc.problems)
			}
		})
	}
}

func TestValidateUnresolvedRef(t *testing.T) {
	c := &Contract{Schemas: map[string]any{}}
	got := c.Validate(decode(t, `{}`), map[string]any{"$ref": "#/components/schemas/Missing"})
	if len(got) != 1 || !strings.Contains(got[0], "unresolved") {
		t.Fatalf("problems = %q", got)
	}
}

func TestLoadContract(t *testing.T) {
	c, err := LoadContract(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, op := range c.Operations {
		if ids[op.ID] {
			t.Errorf("duplicate operationId %s", op.ID)
		}
		ids[op.ID] = true
	}
	// 526 Emby operations plus getHealthz.
	if len(c.Operations) != 527 {
		t.Errorf("operations = %d, want 527", len(c.Operations))
	}
}
