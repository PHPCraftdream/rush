package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

func escapedJSONValue(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded[1 : len(encoded)-1])
}

type apiReadModelSecurityFixture struct {
	ID       string
	Name     string
	Provider string
	APIKey   string
	BaseURL  string
}

type apiReadModelSecurityStore struct {
	models []apiReadModelSecurityFixture
}

func (s apiReadModelSecurityStore) ListSessions(context.Context) ([]session.Session, error) {
	return nil, nil
}

func (s apiReadModelSecurityStore) GetSession(context.Context, string) (session.Session, error) {
	return session.Session{}, sql.ErrNoRows
}

func (s apiReadModelSecurityStore) ListMessages(context.Context, string) ([]message.Message, error) {
	return nil, nil
}

func (s apiReadModelSecurityStore) ListModels(context.Context) ([]APIModel, error) {
	models := make([]APIModel, 0, len(s.models))
	for _, model := range s.models {
		models = append(models, APIModel{ID: model.ID, Name: model.Name, Provider: model.Provider})
	}
	return models, nil
}

func TestAPIReadModelsNeverExposeKeyOrPath(t *testing.T) {
	const apiKey = "fake-provider-api-key-secret"
	const baseURL = `C:\Users\fixture\private\endpoint`
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())

	fixture := apiReadModelSecurityFixture{ID: "safe-model", Name: "Safe", Provider: "fixture", APIKey: apiKey, BaseURL: baseURL}
	store := apiReadModelSecurityStore{models: []apiReadModelSecurityFixture{fixture}}
	handler := NewAPIHandler(APIRoutesWithDeps(store))
	response := apiReadRequest(t, handler, "/api/v1/models", []string{"read"})
	body := response.Body.String()
	for _, secret := range []string{
		fixture.APIKey,
		escapedJSONValue(t, fixture.APIKey),
		fixture.BaseURL,
		escapedJSONValue(t, fixture.BaseURL),
		strings.ReplaceAll(fixture.BaseURL, `\`, "/"),
		escapedJSONValue(t, strings.ReplaceAll(fixture.BaseURL, `\`, "/")),
	} {
		if strings.Contains(body, secret) {
			t.Fatalf("models response contains sensitive value %q: %s", secret, body)
		}
	}
	if response.Code != 200 || !strings.Contains(body, "safe-model") {
		t.Fatalf("status=%d body=%s", response.Code, body)
	}
}
