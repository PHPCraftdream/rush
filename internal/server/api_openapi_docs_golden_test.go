package server

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

var updateOpenAPIDocs = flag.Bool("update-openapi-docs", false, "rewrite docs/api/openapi.json from the served document")

// servedOpenAPIDocument is exactly what GET /api/v1/openapi.json serializes.
func servedOpenAPIDocument(t *testing.T) any {
	t.Helper()
	raw, err := json.Marshal(BuildOpenAPIDocument(APIRoutes()))
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return normalizeOpenAPIVersion(decoded)
}

// normalizeOpenAPIVersion replaces info.version, which embeds the build
// timestamp, so the snapshot is stable across builds.
func normalizeOpenAPIVersion(document any) any {
	if root, ok := document.(map[string]any); ok {
		if info, ok := root["info"].(map[string]any); ok {
			info["version"] = "build-version"
		}
	}
	return document
}

// TestOpenAPIDocsSnapshotMatchesServedDocument pins docs/api/openapi.json to
// the document the server serves: a route added, removed or changed without
// regenerating the file (go test ./internal/server -run OpenAPIDocsSnapshot
// -args -update-openapi-docs) fails here.
func TestOpenAPIDocsSnapshotMatchesServedDocument(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "api", "openapi.json")
	served := servedOpenAPIDocument(t)
	if *updateOpenAPIDocs {
		data, err := json.MarshalIndent(served, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot any
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	if !reflect.DeepEqual(normalizeOpenAPIVersion(snapshot), served) {
		t.Fatalf("%s differs from the served OpenAPI document; regenerate with -args -update-openapi-docs", path)
	}
}
