package tool

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

func parseJSON(raw json.RawMessage, v any) string {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, v); err == nil {
		return ""
	}
	// Lenient fallback: LLMs often emit numeric fields as strings
	// (e.g. {"task_id":"1"}). Coerce numeric-looking string values to numbers
	// and retry.
	if relaxed, relaxErr := coerceNumericStrings(raw); relaxErr == nil {
		if err2 := json.Unmarshal(relaxed, v); err2 == nil {
			return ""
		}
	}
	return "invalid arguments"
}

func coerceNumericStrings(raw json.RawMessage) (json.RawMessage, error) {
	var m any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	coerced := walkAndCoerce(m)
	out, err := json.Marshal(coerced)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}

func walkAndCoerce(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, vv := range val {
			out[k] = walkAndCoerce(vv)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, vv := range val {
			out[i] = walkAndCoerce(vv)
		}
		return out
	case json.Number:
		if i, err := val.Int64(); err == nil {
			return i
		}
		if f, err := val.Float64(); err == nil {
			return f
		}
		return val.String()
	case string:
		// LLMs often replicate "#1" notation from task_create output.
		c := strings.TrimLeft(val, "#")
		if i, err := strconv.ParseInt(c, 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(c, 64); err == nil {
			return f
		}
		return val
	default:
		return val
	}
}
