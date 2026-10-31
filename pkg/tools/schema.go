package tools

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Define 用泛型 + 反射生成工具定义：工具作者只写 Go struct，Schema 自动生成。
func Define[T any](name, description string, fn func(ctx Context, args T) (string, error)) Tool {
	var zero T
	schema, err := SchemaFor(reflect.TypeOf(zero))
	if err != nil {
		panic(fmt.Sprintf("tools: Define(%q): %v", name, err))
	}
	return Tool{
		Name:        name,
		Description: description,
		Schema:      schema,
		Execute: func(ctx Context, raw json.RawMessage) (string, error) {
			var args T
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&args); err != nil {
				return "", fmt.Errorf("参数无效: %w", err)
			}
			return fn(ctx, args)
		},
	}
}

// SchemaFor 由 Go 类型生成 JSON Schema（支持 object/string/int/float/bool/array/map/嵌套指针）。
//
// 刻意只支持一个可控子集：不支持的类型在启动期 panic，而不是运行期出怪问题。
func SchemaFor(t reflect.Type) (json.RawMessage, error) {
	schema, err := schemaNode(t, 0)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func schemaNode(t reflect.Type, depth int) (map[string]any, error) {
	if depth > 8 {
		return nil, fmt.Errorf("schema 嵌套过深（>8），请拆小结构体")
	}
	if t.Kind() == reflect.Pointer {
		return schemaNode(t.Elem(), depth+1)
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil
	case reflect.Slice:
		item, err := schemaNode(t.Elem(), depth+1)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": item}, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("只支持 string 作为 map key，实际 %s", t.Key())
		}
		val, err := schemaNode(t.Elem(), depth+1)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": val}, nil
	case reflect.Struct:
		props := map[string]any{}
		var required []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := f.Tag.Get("json")
			name, opts, _ := strings.Cut(tag, ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			node, err := schemaNode(f.Type, depth+1)
			if err != nil {
				return nil, fmt.Errorf("字段 %s: %w", f.Name, err)
			}
			if doc := f.Tag.Get("jsonschema"); doc != "" {
				node["description"] = doc
			}
			props[name] = node
			if !strings.Contains(opts, "omitempty") {
				required = append(required, name)
			}
		}
		out := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			out["required"] = required
		}
		return out, nil
	}
	return nil, fmt.Errorf("不支持的类型 %s", t)
}
