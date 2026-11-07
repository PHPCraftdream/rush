package agentresult

import (
	"errors"
	"net/url"
	"sort"
	"strings"
)

const (
	maxSchemaRefDepth = 32
	maxSchemaRefNodes = 2048
)

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func collectSchemaRefs(root map[string]any) (map[string]map[string]any, error) {
	registry := make(map[string]map[string]any)
	nodes := 0
	var collect func(map[string]any, string, int) error
	collect = func(schema map[string]any, pointer string, depth int) error {
		if depth > maxSchemaRefDepth {
			return errors.New("schema depth")
		}
		nodes++
		if nodes > maxSchemaRefNodes {
			return errors.New("schema nodes")
		}
		registry[pointer] = schema
		for _, key := range []string{"properties", "$defs"} {
			children, ok := schema[key].(map[string]any)
			if !ok {
				continue
			}
			for _, name := range sortedKeys(children) {
				child, ok := children[name].(map[string]any)
				if !ok {
					continue
				}
				if err := collect(child, schemaChildPointer(schemaChildPointer(pointer, key), name), depth+1); err != nil {
					return err
				}
			}
		}
		for _, key := range []string{"items", "additionalProperties"} {
			child, ok := schema[key].(map[string]any)
			if !ok {
				continue
			}
			if err := collect(child, schemaChildPointer(pointer, key), depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := collect(root, "", 1); err != nil {
		return nil, err
	}
	return registry, nil
}

func schemaChildPointer(parent, key string) string {
	return parent + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

func resolveSchemaRef(registry map[string]map[string]any, ref string) (map[string]any, string, bool) {
	u, err := url.Parse(ref)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Path != "" || u.RawQuery != "" || !strings.HasPrefix(u.Fragment, "/$defs/") {
		return nil, "", false
	}
	fragment, err := url.PathUnescape(u.EscapedFragment())
	if err != nil || !strings.HasPrefix(fragment, "/$defs/") {
		return nil, "", false
	}
	parts := strings.Split(strings.TrimPrefix(fragment, "/"), "/")
	decoded := make([]string, len(parts))
	for i, part := range parts {
		var b strings.Builder
		for j := 0; j < len(part); j++ {
			if part[j] != '~' {
				b.WriteByte(part[j])
				continue
			}
			if j+1 >= len(part) || (part[j+1] != '0' && part[j+1] != '1') {
				return nil, "", false
			}
			j++
			if part[j] == '0' {
				b.WriteByte('~')
			} else {
				b.WriteByte('/')
			}
		}
		decoded[i] = b.String()
	}
	pointer := ""
	for _, part := range decoded {
		pointer = schemaChildPointer(pointer, part)
	}
	target, ok := registry[pointer]
	if !ok {
		return nil, "", false
	}
	return target, pointer, true
}
