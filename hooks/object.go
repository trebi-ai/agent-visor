package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// object is a JSON object that keeps the order of its keys and the bytes of
// its values. An edit of one key keeps the rest of a user config as it was.
type object []member

type member struct {
	Key   string
	Value json.RawMessage
}

// parseObject reads a JSON object. Empty input gives an empty object.
func parseObject(data []byte) (object, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return object{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("the config is not a JSON object")
	}
	o := object{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string) //nolint:errcheck // an object key is always a string
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o = append(o, member{key, v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o object) get(key string) (json.RawMessage, bool) {
	for _, m := range o {
		if m.Key == key {
			return m.Value, true
		}
	}
	return nil, false
}

// set replaces the value of key in place, or adds key at the end.
func (o object) set(key string, v json.RawMessage) object {
	for i := range o {
		if o[i].Key == key {
			o[i].Value = v
			return o
		}
	}
	return append(o, member{key, v})
}

func (o object) without(key string) object {
	out := object{}
	for _, m := range o {
		if m.Key != key {
			out = append(out, m)
		}
	}
	return out
}

// compact gives the object with no spaces. Strings keep their escapes.
func (o object) compact() (json.RawMessage, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := marshal(m.Key)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		if err := json.Compact(&b, m.Value); err != nil {
			return nil, fmt.Errorf("key %q: %w", m.Key, err)
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// file gives the object with an indent of two spaces and a last newline, as
// the vendor CLIs write it.
func (o object) file() ([]byte, error) {
	raw, err := o.compact()
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return nil, err
	}
	b.WriteByte('\n')
	return b.Bytes(), nil
}

// marshal is json.Marshal with no HTML escapes, so "&" and "<" in a shell
// command stay as they are.
func marshal(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}
