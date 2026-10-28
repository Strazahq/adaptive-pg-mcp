package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"

	"github.com/google/jsonschema-go/jsonschema"
)

// argsOf reads the arguments of a call of tool as a JSON object whose values
// are decoded with json.Number, so that no integer loses digits. No arguments
// at all read as an empty object.
func argsOf(tool string, raw json.RawMessage) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return map[string]any{}, nil
	}
	v, err := argsValue(raw)
	if err != nil {
		return nil, argError(tool, "the arguments are not valid JSON")
	}
	args, ok := v.(map[string]any)
	if !ok {
		return nil, argError(tool, "the arguments are "+kindOf(v)+", not a JSON object")
	}
	return args, nil
}

// argsValue decodes one JSON value with json.Number, so that no integer loses digits.
func argsValue(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

// argError is the refusal of a call whose arguments do not match the input schema of tool.
func argError(tool, why string) error {
	return errors.New(tool + " refused the call, because " + why + ". Correct the arguments as the input schema of " + tool + " describes them, and call it again.")
}

// checkArgs checks args against in, the input schema of tool, and names the first mismatch.
func checkArgs(tool string, in *jsonschema.Schema, args map[string]any) error {
	if why := mismatch(in, args, ""); why != "" {
		return argError(tool, why)
	}
	return nil
}

// mismatch checks v against s, within the part of JSON Schema that the
// built-in tools use, and says why v does not match, or answers empty. at
// names v in the sentence, and is empty for the arguments themselves.
func mismatch(s *jsonschema.Schema, v any, at string) string {
	if s == nil {
		return ""
	}
	name := "the argument " + at
	if at == "" {
		name = "the arguments"
	}
	if s.Type != "" && !hasType(v, s.Type) {
		return name + " must be " + article(s.Type) + ", and the call passed " + kindOf(v)
	}
	if len(s.Enum) > 0 && !slices.Contains(s.Enum, v) {
		return name + " must be one of " + enumText(s.Enum) + ", and the call passed " + valueText(v)
	}
	switch x := v.(type) {
	case string:
		if s.MinLength != nil && len(x) < *s.MinLength {
			return name + " must not be empty"
		}
	case json.Number:
		f, _ := x.Float64()
		if s.Minimum != nil && f < *s.Minimum {
			return name + " must be at least " + number(*s.Minimum) + ", and the call passed " + x.String()
		}
		if s.Maximum != nil && f > *s.Maximum {
			return name + " must be at most " + number(*s.Maximum) + ", and the call passed " + x.String()
		}
	case []any:
		if s.MinItems != nil && len(x) < *s.MinItems {
			return name + " must hold at least " + entries(*s.MinItems) + ", and the call passed " + entries(len(x))
		}
		for i, item := range x {
			if why := mismatch(s.Items, item, at+"["+strconv.Itoa(i)+"]"); why != "" {
				return why
			}
		}
	case map[string]any:
		return objectMismatch(s, x, at, name)
	}
	return ""
}

// objectMismatch checks an object against s: its required keys, its closed set of keys, and each value.
func objectMismatch(s *jsonschema.Schema, x map[string]any, at, name string) string {
	key := func(k string) string {
		if at == "" {
			return k
		}
		return at + "." + k
	}
	for _, r := range s.Required {
		if _, ok := x[r]; !ok {
			if at == "" {
				return "it needs the argument " + r + ", and the call leaves it out"
			}
			return name + " needs " + r + ", and the call leaves it out"
		}
	}
	closed := s.AdditionalProperties != nil && s.AdditionalProperties.Not != nil
	for _, k := range slices.Sorted(maps.Keys(x)) {
		p, known := s.Properties[k]
		if !known && closed {
			if at == "" {
				return "it takes no argument named " + strconv.Quote(k)
			}
			return name + " has the key " + strconv.Quote(k) + ", which it does not take"
		}
		if !known {
			p = s.AdditionalProperties
		}
		if why := mismatch(p, x[k], key(k)); why != "" {
			return why
		}
	}
	if s.MinProperties != nil && len(x) < *s.MinProperties {
		return name + " must name at least " + strconv.Itoa(*s.MinProperties) + " column, and the call passed none"
	}
	return ""
}

// hasType reports whether v, decoded with json.Number, has the JSON Schema type t.
func hasType(v any, t string) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		_, err := strconv.ParseInt(n.String(), 10, 64)
		return err == nil
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	}
	return false
}

// kindOf names the JSON kind of v for a sentence, such as a string or null.
func kindOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case json.Number:
		if hasType(x, "integer") {
			return "an integer"
		}
		return "a number"
	case []any:
		return "a list"
	case map[string]any:
		return "an object"
	}
	return "a value of another kind"
}

// article writes a JSON Schema type with its article, such as an integer.
func article(t string) string {
	switch t {
	case "integer", "array", "object":
		return map[string]string{"integer": "an integer", "array": "a list", "object": "an object"}[t]
	}
	return "a " + t
}

// enumText writes the values of an enum as a, b and c.
func enumText(values []any) string {
	names := make([]string, len(values))
	for i, v := range values {
		names[i] = valueText(v)
	}
	return joinAnd(names)
}

// valueText writes a JSON value for a sentence, with a string in quotes.
func valueText(v any) string {
	switch x := v.(type) {
	case string:
		return strconv.Quote(x)
	case json.Number:
		return x.String()
	}
	return kindOf(v)
}

// number writes a bound of a schema, such as 1,000.
func number(f float64) string { return count(int64(f)) }

// entries writes a count of list entries, such as 1 entry.
func entries(n int) string {
	if n == 1 {
		return "1 entry"
	}
	if n == 0 {
		return "none"
	}
	return count(int64(n)) + " entries"
}

// plural writes n and the noun, as 1 row or 1,240 rows.
func plural(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return count(n) + " " + noun + "s"
}
