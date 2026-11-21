package matchi

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"dllio/backend/jsonx"
)

// Limit은 배열 필드의 항목 수 하한·상한이다. 음수이면 제한하지 않는다.
type Limit struct {
	Min, Max int
}

// SchemaOf는 구조체 T의 JSON 출력 형식이다. 항목 설명은 desc, 고정된 보기는 enum(쉼표로 구분), 목록 길이는 min과 max 태그로 적는다.
// 값이 민원마다 달라지는 보기는 enums에, 목록 길이는 limits에 "형식이름.필드이름"으로 넘긴다. 포인터 필드는 null을 허용한다.
func SchemaOf[T any](enums map[string][]string, limits ...map[string]Limit) (jsonx.Object, error) {
	var lim map[string]Limit
	if len(limits) > 0 {
		lim = limits[0]
	}
	b := schemaBuilder{enums: enums, limits: lim, defs: map[string]jsonx.Object{}}
	root, err := b.object(reflect.TypeFor[T]())
	if err != nil {
		return nil, err
	}
	if len(b.defs) > 0 {
		names := make([]string, 0, len(b.defs))
		for name := range b.defs {
			names = append(names, name)
		}
		sort.Strings(names)
		defs := jsonx.Object{}
		for _, name := range names {
			defs = append(defs, jsonx.Field{Key: name, Value: b.defs[name]})
		}
		root = append(root, jsonx.Field{Key: "$defs", Value: defs})
	}
	return root, nil
}

type schemaBuilder struct {
	enums  map[string][]string
	limits map[string]Limit
	defs   map[string]jsonx.Object
}

func (b *schemaBuilder) object(t reflect.Type) (jsonx.Object, error) {
	properties, required := jsonx.Object{}, []string{}
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		// json:"-" 필드는 모델이 쓰지 않고 코드가 채운다.
		if name == "-" {
			continue
		}
		if name == "" {
			return nil, fmt.Errorf("%s.%s에 json 이름이 없다", t.Name(), f.Name)
		}
		schema, err := b.field(t, f, name)
		if err != nil {
			return nil, err
		}
		properties = append(properties, jsonx.Field{Key: name, Value: schema})
		required = append(required, name)
	}
	return jsonx.Object{{Key: "properties", Value: properties}, {Key: "required", Value: required}, {Key: "type", Value: "object"}, {Key: "additionalProperties", Value: false}}, nil
}

func (b *schemaBuilder) field(owner reflect.Type, f reflect.StructField, name string) (jsonx.Object, error) {
	var value jsonx.Object
	key := owner.Name() + "." + name
	enum := b.enums[key]
	if tag := f.Tag.Get("enum"); tag != "" {
		enum = strings.Split(tag, ",")
	}
	desc := jsonx.Object{}
	if text := f.Tag.Get("desc"); text != "" {
		desc = jsonx.Object{{Key: "description", Value: text}}
	}
	// 보기가 하나뿐이면 const로 적는다.
	if len(enum) == 1 {
		return append(append(jsonx.Object{{Key: "const", Value: enum[0]}}, desc...), jsonx.Field{Key: "type", Value: "string"}), nil
	}
	if enum != nil {
		return append(desc, jsonx.Field{Key: "enum", Value: enum}, jsonx.Field{Key: "type", Value: "string"}), nil
	}
	value, err := b.value(f.Type, f, key)
	if err != nil {
		return nil, err
	}
	return append(desc, value...), nil
}

func (b *schemaBuilder) value(t reflect.Type, f reflect.StructField, key string) (jsonx.Object, error) {
	switch t.Kind() {
	case reflect.String:
		// maxlen 태그는 글자 수 상한이다.
		if tag := f.Tag.Get("maxlen"); tag != "" {
			n, err := strconv.Atoi(tag)
			if err != nil {
				return nil, err
			}
			return jsonx.Object{{Key: "maxLength", Value: n}, {Key: "type", Value: "string"}}, nil
		}
		return jsonx.Object{{Key: "type", Value: "string"}}, nil
	case reflect.Bool:
		return jsonx.Object{{Key: "type", Value: "boolean"}}, nil
	case reflect.Int:
		return jsonx.Object{{Key: "type", Value: "integer"}}, nil
	case reflect.Float64:
		return jsonx.Object{{Key: "type", Value: "number"}}, nil
	case reflect.Pointer:
		inner, err := b.value(t.Elem(), f, key)
		if err != nil {
			return nil, err
		}
		return jsonx.Object{{Key: "anyOf", Value: []any{inner, jsonx.Object{{Key: "type", Value: "null"}}}}}, nil
	case reflect.Slice:
		items, err := b.value(t.Elem(), reflect.StructField{}, "")
		if err != nil {
			return nil, err
		}
		out := jsonx.Object{{Key: "items", Value: items}}
		if lim, ok := b.limits[key]; ok {
			if lim.Max >= 0 {
				out = append(out, jsonx.Field{Key: "maxItems", Value: lim.Max})
			}
			if lim.Min >= 0 {
				out = append(out, jsonx.Field{Key: "minItems", Value: lim.Min})
			}
		} else {
			for _, limit := range [][2]string{{"max", "maxItems"}, {"min", "minItems"}} {
				if tag := f.Tag.Get(limit[0]); tag != "" {
					n, err := strconv.Atoi(tag)
					if err != nil {
						return nil, err
					}
					out = append(out, jsonx.Field{Key: limit[1], Value: n})
				}
			}
		}
		return append(out, jsonx.Field{Key: "type", Value: "array"}), nil
	case reflect.Struct:
		if _, seen := b.defs[t.Name()]; !seen {
			b.defs[t.Name()] = nil
			def, err := b.object(t)
			if err != nil {
				return nil, err
			}
			b.defs[t.Name()] = def
		}
		return jsonx.Object{{Key: "$ref", Value: "#/$defs/" + t.Name()}}, nil
	}
	return nil, fmt.Errorf("출력 형식으로 쓸 수 없는 형식: %s", t)
}

// validate는 값이 출력 형식에 맞는지 보고 어긋난 곳을 돌려준다.
func validate(schema jsonx.Object, value any) ([]string, error) {
	data, err := jsonx.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	defs, _ := root["$defs"].(map[string]any)
	var found []string
	walk(root, defs, value, "출력", &found)
	return found, nil
}

func walk(s, defs map[string]any, v any, path string, found *[]string) {
	if ref, ok := s["$ref"].(string); ok {
		s = defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
	}
	if alternatives, ok := s["anyOf"].([]any); ok {
		for _, alternative := range alternatives {
			var inner []string
			walk(alternative.(map[string]any), defs, v, path, &inner)
			if len(inner) == 0 {
				return
			}
		}
		*found = append(*found, path+": 허용된 형식이 아니다")
		return
	}
	switch s["type"] {
	case "object":
		object, ok := v.(map[string]any)
		if !ok {
			*found = append(*found, path+": 객체가 아니다")
			return
		}
		properties := s["properties"].(map[string]any)
		for _, name := range s["required"].([]any) {
			key := name.(string)
			item, ok := object[key]
			if !ok {
				*found = append(*found, path+"."+key+": 없다")
				continue
			}
			walk(properties[key].(map[string]any), defs, item, path+"."+key, found)
		}
		for key := range object {
			if _, ok := properties[key]; !ok {
				*found = append(*found, path+"."+key+": 정의되지 않은 항목이다")
			}
		}
	case "array":
		list, ok := v.([]any)
		if !ok {
			*found = append(*found, path+": 목록이 아니다")
			return
		}
		if limit, ok := s["minItems"].(float64); ok && float64(len(list)) < limit {
			*found = append(*found, fmt.Sprintf("%s: 항목이 %d개 이상이어야 한다", path, int(limit)))
		}
		if limit, ok := s["maxItems"].(float64); ok && float64(len(list)) > limit {
			*found = append(*found, fmt.Sprintf("%s: 항목이 %d개 이하여야 한다", path, int(limit)))
		}
		for i, item := range list {
			walk(s["items"].(map[string]any), defs, item, fmt.Sprintf("%s[%d]", path, i), found)
		}
	case "string":
		text, ok := v.(string)
		if !ok {
			*found = append(*found, path+": 글이 아니다")
			return
		}
		if fixed, ok := s["const"].(string); ok && fixed != text {
			*found = append(*found, fmt.Sprintf("%s: «%s»가 아니라 «%s»여야 한다", path, text, fixed))
		}
		if enum, ok := s["enum"].([]any); ok {
			for _, option := range enum {
				if option == text {
					return
				}
			}
			*found = append(*found, fmt.Sprintf("%s: «%s»는 보기에 없다", path, text))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			*found = append(*found, path+": 참·거짓이 아니다")
		}
	case "integer", "number":
		if _, ok := v.(float64); !ok {
			*found = append(*found, path+": 숫자가 아니다")
		}
	case "null":
		if v != nil {
			*found = append(*found, path+": null이 아니다")
		}
	}
}
