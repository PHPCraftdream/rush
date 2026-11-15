package lab

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

func scenarioCheckJSON(data json.RawMessage, typ reflect.Type) error {
	if typ == reflect.TypeFor[json.RawMessage]() {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return scenarioInvalid("null field")
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return scenarioInvalid("object")
		}
		known := make(map[string]reflect.Type)
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			known[strings.Split(f.Tag.Get("json"), ",")[0]] = f.Type
		}
		for key, value := range fields {
			fieldType, ok := known[key]
			if !ok {
				return scenarioInvalid("unknown field")
			}
			if err := scenarioCheckJSON(value, fieldType); err != nil {
				return err
			}
		}
	case reflect.Map:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return scenarioInvalid("map")
		}
		for _, value := range fields {
			if err := scenarioCheckJSON(value, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return scenarioInvalid("array")
		}
		if typ.Kind() == reflect.Array && len(values) != typ.Len() {
			return scenarioInvalid("array length")
		}
		for _, value := range values {
			if err := scenarioCheckJSON(value, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
