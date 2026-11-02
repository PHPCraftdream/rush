package server

import (
	"encoding/json"
	"testing"
)

func TestSessionOpenAPIContract(t *testing.T) {
	document := BuildOpenAPIDocument(APIRoutes())
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	paths := decoded["paths"].(map[string]any)
	cases := []struct {
		path, method, operationID, success string
		params                             map[string]bool
	}{
		{"/sessions", "post", "createSession", "201", map[string]bool{"Idempotency-Key": false}},
		{"/sessions/{id}/fork", "post", "forkSession", "201", map[string]bool{"id": true, "Idempotency-Key": false}},
		{"/sessions/{id}", "delete", "deleteSession", "204", map[string]bool{"id": true}},
		{"/sessions", "get", "listSessions", "200", map[string]bool{"after": false, "limit": false}},
		{"/sessions/{id}", "get", "getSession", "200", map[string]bool{"id": true}},
		{"/sessions/{id}/messages", "get", "listMessages", "200", map[string]bool{"id": true, "after": false, "limit": false}},
	}
	for _, tc := range cases {
		t.Run(tc.operationID, func(t *testing.T) {
			item, ok := paths[tc.path].(map[string]any)
			if !ok {
				t.Fatalf("missing path %s", tc.path)
			}
			op, ok := item[tc.method].(map[string]any)
			if !ok {
				t.Fatalf("missing %s operation on %s", tc.method, tc.path)
			}
			if op["operationId"] != tc.operationID {
				t.Fatalf("operationId=%v", op["operationId"])
			}
			responses := op["responses"].(map[string]any)
			if _, ok := responses[tc.success]; !ok {
				t.Fatalf("missing success response %s", tc.success)
			}
			for _, status := range []string{"200", "201", "204"} {
				if status != tc.success {
					if _, ok := responses[status]; ok {
						t.Errorf("incorrect success response %s is documented", status)
					}
				}
			}
			for _, status := range []string{"401", "403", "404", "405", "409", "422", "429", "500"} {
				response, ok := responses[status].(map[string]any)
				if !ok {
					t.Errorf("missing problem response %s", status)
					continue
				}
				content := response["content"].(map[string]any)
				problem := content["application/problem+json"].(map[string]any)
				if problem["schema"].(map[string]any)["$ref"] != "#/components/schemas/Problem" {
					t.Errorf("response %s does not reference Problem", status)
				}
			}
			gotParams := map[string]bool{}
			if rawParams, exists := op["parameters"]; exists {
				for _, raw := range rawParams.([]any) {
					param := raw.(map[string]any)
					name := param["name"].(string)
					gotParams[name] = param["required"].(bool)
					if name == "Idempotency-Key" && param["in"] != "header" {
						t.Errorf("Idempotency-Key location=%v", param["in"])
					}
				}
			}
			if len(gotParams) != len(tc.params) {
				t.Errorf("parameters=%v want %v", gotParams, tc.params)
			}
			for name, required := range tc.params {
				gotRequired, exists := gotParams[name]
				if !exists {
					t.Errorf("missing parameter %s", name)
					continue
				}
				if gotRequired != required {
					t.Errorf("parameter %s required=%v want %v", name, gotRequired, required)
				}
			}
		})
	}
}
