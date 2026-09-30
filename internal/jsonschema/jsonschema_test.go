package jsonschema

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestObjectNeverCarriesNull(t *testing.T) {
	var required []string
	var nested []map[string]any
	schema := map[string]any{
		"type":       "object",
		"required":   required,
		"properties": map[string]any{"id": map[string]any{"type": "string", "enum": []string(nil), "description": nil, "maxLength": 12}},
		"anyOf":      nested,
		"allOf":      []map[string]any{{"required": []string(nil), "title": nil}},
		"$defs":      map[string]any(nil),
	}
	out, err := Object(schema)
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(out)
	if strings.Contains(string(wire), "null") {
		t.Fatalf("null reached the wire: %s", wire)
	}
	want := `{"$defs":{},"allOf":[{}],"properties":{"id":{"enum":[],"maxLength":12,"type":"string"}},"required":[],"type":"object"}`
	if string(wire) != want {
		t.Fatalf("got %s\nwant %s", wire, want)
	}
	id := schema["properties"].(map[string]any)["id"].(map[string]any)
	if _, kept := id["description"]; !kept || schema["required"].([]string) != nil {
		t.Fatal("the caller's schema was modified")
	}
}

func TestObjectRefusesWhatCannotBeArguments(t *testing.T) {
	for name, tc := range map[string]struct {
		schema map[string]any
		want   error
	}{
		"array root":   {map[string]any{"type": "array"}, ErrNotObject},
		"union root":   {map[string]any{"type": []string{"object", "null"}}, ErrNotObject},
		"unencodable":  {map[string]any{"type": "object", "default": func() {}}, ErrUnencodable},
		"typeless ok":  {map[string]any{"properties": map[string]any{}}, nil},
		"object is ok": {map[string]any{"type": "object"}, nil},
	} {
		if _, err := Object(tc.schema); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
