// Package jsonschema prepares a caller's tool argument schema for a harness or
// endpoint that will parse it. Callers build schemas as Go values, and Go
// spells an absent list as null; a schema keyword is never null, and the
// parsers on the other side reject a whole tool list for one. Claude Code
// 2.1.283 dropped every hosted tool over "required": null, and strict API
// endpoints refuse the request. One copy keeps completion and sessions from
// disagreeing about what reaches the wire.
package jsonschema

import (
	"bytes"
	"encoding/json"
	"errors"
)

var (
	// ErrUnencodable: the schema holds a value that has no JSON form.
	ErrUnencodable = errors.New("tool argument schema is not encodable as JSON")
	// ErrNotObject: tool arguments are always a JSON object, so a schema whose
	// root describes anything else can never be satisfied.
	ErrNotObject = errors.New(`tool argument schema must describe an object ("type": "object")`)
)

// Object returns a deep copy of a tool's argument schema in plain JSON values:
// objects, arrays, strings, json.Number, booleans. A nil list becomes an empty
// one, so a tool with no required arguments says "required": []; any other
// null keyword is left out. The caller's value is never modified.
func Object(schema map[string]any) (map[string]any, error) {
	data, err := json.Marshal(lists(schema))
	if err != nil {
		return nil, ErrUnencodable
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var root map[string]any
	if decoder.Decode(&root) != nil || root == nil {
		return nil, ErrUnencodable
	}
	root = withoutNulls(root).(map[string]any)
	if kind, ok := root["type"]; ok && kind != "object" {
		return nil, ErrNotObject
	}
	return root, nil
}

// lists turns the nil lists a Go caller naturally writes into empty ones
// before encoding, where they would otherwise become null.
func lists(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, nested := range typed {
			out[key] = lists(nested)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, nested := range typed {
			out = append(out, lists(nested))
		}
		return out
	case []string:
		return append([]string{}, typed...)
	default:
		return value
	}
}

// withoutNulls leaves out null keywords at every depth, including those only
// encoding revealed, such as a typed nil map or slice of maps.
func withoutNulls(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if nested == nil {
				delete(typed, key)
				continue
			}
			typed[key] = withoutNulls(nested)
		}
		return typed
	case []any:
		for i, nested := range typed {
			typed[i] = withoutNulls(nested)
		}
		return typed
	default:
		return value
	}
}
