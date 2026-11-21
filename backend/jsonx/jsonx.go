// Package jsonx는 키 순서를 지키는 JSON 객체, 사람이 읽는 파일 쓰기, 정렬된 형식의 hash를 둔다.
package jsonx

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Field는 Object의 한 항목이다.
type Field struct {
	Key   string
	Value any
}

// Object는 넣은 순서대로 쓰는 JSON 객체다.
type Object []Field

// MarshalJSON은 키를 넣은 순서대로 쓴다.
func (o Object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := Marshal(f.Key)
		if err != nil {
			return nil, err
		}
		value, err := Marshal(f.Value)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Marshal은 한글과 <, >, &를 이스케이프하지 않고 JSON으로 쓴다.
func Marshal(value any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// WriteFile은 값을 한 칸 들여쓰기 JSON 파일로 쓴다. 폴더가 없으면 만든다.
func WriteFile(path string, value any) error {
	data, err := Marshal(value)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	if err := json.Indent(&b, data, "", " "); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}

// Parse는 숫자의 글자를 그대로 둔 채 JSON을 읽는다.
func Parse(data []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

// J는 적힌 그대로 읽은 JSON 객체다. 숫자는 json.Number다.
type J = map[string]any

// Str는 글이면 그 글, 아니면 빈 글이다.
func Str(v any) string { s, _ := v.(string); return s }

// List는 목록이면 그 목록, 아니면 nil이다.
func List(v any) []any { l, _ := v.([]any); return l }

// Obj는 객체면 그 객체, 아니면 nil이다. 순서 있는 Object는 같은 키와 값의 J로 바꿔 돌려준다.
func Obj(v any) J {
	switch o := v.(type) {
	case J:
		return o
	case Object:
		out := make(J, len(o))
		for _, f := range o {
			out[f.Key] = f.Value
		}
		return out
	}
	return nil
}

// Strs는 글 목록이다.
func Strs(v any) []string {
	out := []string{}
	for _, x := range List(v) {
		out = append(out, Str(x))
	}
	return out
}

// Num은 숫자 값이다.
func Num(v any) float64 {
	switch n := v.(type) {
	case json.Number:
		f, _ := n.Float64()
		return f
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}

// Ints는 정수 목록이다.
func Ints(v any) []int {
	out := []int{}
	for _, x := range List(v) {
		out = append(out, int(Num(x)))
	}
	return out
}

// Generic은 값을 JSON으로 한 번 옮겨 적힌 그대로 읽은 값(J, []any, json.Number)으로 바꾼다.
func Generic(value any) (any, error) {
	raw, err := Marshal(value)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// ParseOrdered는 객체를 키 순서를 지킨 Object로 읽는다. 숫자는 json.Number다.
func ParseOrdered(data []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	return readOrdered(d)
}

func readOrdered(d *json.Decoder) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return t, nil
	}
	switch delim {
	case '{':
		out := Object{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			value, err := readOrdered(d)
			if err != nil {
				return nil, err
			}
			out = append(out, Field{key.(string), value})
		}
		_, err := d.Token()
		return out, err
	case '[':
		out := []any{}
		for d.More() {
			value, err := readOrdered(d)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		_, err := d.Token()
		return out, err
	}
	return nil, fmt.Errorf("JSON을 읽지 못했다: %v", delim)
}

// Get은 Object의 키 값이다. 없으면 nil이다.
func (o Object) Get(key string) any {
	for _, f := range o {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

// Has는 Object에 키가 있는지다.
func (o Object) Has(key string) bool {
	for _, f := range o {
		if f.Key == key {
			return true
		}
	}
	return false
}

// RawField는 JSON 객체의 항목 하나를 읽지 않은 값과 함께 담는다.
type RawField struct {
	Key   string
	Value json.RawMessage
}

// Fields는 JSON 객체의 항목을 파일에 적힌 순서대로 돌려준다.
func Fields(data []byte) ([]RawField, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	if t, err := d.Token(); err != nil {
		return nil, err
	} else if t != json.Delim('{') {
		return nil, fmt.Errorf("JSON 객체가 아니다")
	}
	var out []RawField
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil, err
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, err
		}
		out = append(out, RawField{t.(string), value})
	}
	return out, nil
}

// SHA는 글자의 SHA-256이다.
func SHA(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Digest는 키를 정렬하고 ", "와 ": "로 이어 쓴 글의 SHA-256이다. Parse로 읽은 값을 받는다.
func Digest(value any) (string, error) {
	var b strings.Builder
	if err := canonical(&b, value); err != nil {
		return "", err
	}
	return SHA(b.String()), nil
}

func canonical(b *strings.Builder, value any) error {
	switch v := value.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case json.Number:
		b.WriteString(v.String())
	case string:
		writeString(b, v)
	case []any:
		b.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := canonical(b, item); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			writeString(b, k)
			b.WriteString(": ")
			if err := canonical(b, v[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("hash할 수 없는 값: %T", value)
	}
	return nil
}

func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\f':
			b.WriteString(`\f`)
		case '\b':
			b.WriteString(`\b`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}
