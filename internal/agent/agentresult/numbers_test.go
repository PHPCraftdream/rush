package agentresult

import (
	"encoding/json"
	"testing"
)

func TestNumberFloatRoundTrip(t *testing.T) {
	for _, s := range []string{"0.1", "9007199254740991", "9007199254740991.0", "-9007199254740991", "0e999999999999999999999999", "1e0000000000000001"} {
		if _, err := numberFloat(json.Number(s)); err != nil {
			t.Errorf("%s rejected: %v", s, err)
		}
	}
	for _, s := range []string{"0.10000000000000001", "9007199254740992", "9007199254740992.0", "9.007199254740992e15", "1e99999999999999999999999", "1e-9999999999999999999999"} {
		if _, err := numberFloat(json.Number(s)); err == nil {
			t.Errorf("%s accepted", s)
		}
	}
}

func TestNumberSchemaEnumConst(t *testing.T) {
	if _, err := DecodeSpec([]byte(`{"type":"json_schema","schema":{"const":0e999999999999999999999999}}`)); err != nil {
		t.Errorf("huge zero const rejected: %v", err)
	}
	for _, keyword := range []string{"enum", "const"} {
		for _, num := range []string{"0.1", "0.10000000000000001", "9007199254740992.0", "0e999999999999999999999999"} {
			value := num
			if keyword == "enum" {
				value = "[" + num + "]"
			}
			raw := []byte(`{"type":"json_schema","schema":{"` + keyword + `":` + value + `}}`)
			_, err := DecodeSpec(raw)
			wantErr := num == "0.10000000000000001" || num == "9007199254740992.0"
			if (err != nil) != wantErr {
				t.Errorf("%s %s: err=%v", keyword, num, err)
			}
		}
	}
}
