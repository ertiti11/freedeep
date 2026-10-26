// Package ojson decodes JSON into a tree that preserves object key order.
//
// encoding/json decodes objects into map[string]any, which re-sorts keys on
// output. Tool schemas and tool-call arguments are echoed back to clients and
// to the model, so they should round-trip in their original order.
package ojson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Member is one key/value pair of an Object.
type Member struct {
	Key   string
	Value any
}

// Object is an ordered JSON object. Values are nil, bool, string,
// json.Number, []any, or Object.
type Object []Member

// Get returns the value for key and whether it was present.
func (o Object) Get(key string) (any, bool) {
	for _, m := range o {
		if m.Key == key {
			return m.Value, true
		}
	}
	return nil, false
}

// Has reports whether key is present.
func (o Object) Has(key string) bool {
	_, ok := o.Get(key)
	return ok
}

// Set replaces key in place or appends it.
func (o *Object) Set(key string, v any) {
	for i := range *o {
		if (*o)[i].Key == key {
			(*o)[i].Value = v
			return
		}
	}
	*o = append(*o, Member{key, v})
}

// MarshalJSON writes members in order.
func (o Object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(m.Key)
		buf.Write(k)
		buf.WriteByte(':')
		v, err := Marshal(m.Value)
		if err != nil {
			return nil, err
		}
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Marshal encodes v without HTML escaping (matching JSON.stringify).
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// MarshalString is Marshal returning a string ("" on error).
func MarshalString(v any) string {
	b, err := Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// Parse decodes exactly one JSON value from data.
func Parse(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("ojson: trailing data after JSON value")
	}
	return v, nil
}

// ParseString is Parse for strings.
func ParseString(s string) (any, error) { return Parse([]byte(s)) }

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := Object{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("ojson: invalid object key %v", kt)
				}
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				// JSON.parse keeps the last duplicate; mirror that.
				obj.Set(key, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("ojson: unexpected delimiter %v", t)
	default:
		return t, nil
	}
}

// RawObject parses raw as an ordered object; ok is false for anything else.
func RawObject(raw []byte) (Object, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false
	}
	v, err := Parse(raw)
	if err != nil {
		return nil, false
	}
	o, ok := v.(Object)
	return o, ok
}
