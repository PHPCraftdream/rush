package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

func decodeAPIResponse(response *httptest.ResponseRecorder, target any) error {
	return json.Unmarshal(response.Body.Bytes(), target)
}

func TestAPIReadMessagesPaginationThreePages(t *testing.T) {
	fixture := apiReadFixture{sessions: []session.Session{{ID: "s"}}}
	for i := 0; i < 120; i++ {
		fixture.messages = append(fixture.messages, message.Message{ID: fmt.Sprintf("m-%03d", i), SessionID: "s", Role: message.User, CreatedAt: int64(i)})
	}
	handler := NewAPIHandler(APIRoutesWithDeps(fixture))
	for page, after := range []string{"", "m-049", "m-099"} {
		path := "/api/v1/sessions/s/messages?limit=50"
		if after != "" {
			path += "&after=" + after
		}
		response := apiReadRequest(t, handler, path, []string{"read"})
		var got struct {
			Items     []APIMessage `json:"items"`
			NextAfter any          `json:"next_after"`
		}
		if response.Code != http.StatusOK || decodeAPIResponse(response, &got) != nil {
			t.Fatalf("page=%d status=%d body=%s", page, response.Code, response.Body.String())
		}
		wantCount := 50
		if page == 2 {
			wantCount = 20
		}
		if len(got.Items) != wantCount {
			t.Fatalf("page=%d count=%d want=%d", page, len(got.Items), wantCount)
		}
		for i, item := range got.Items {
			if wantID := fmt.Sprintf("m-%03d", page*50+i); item.ID != wantID {
				t.Errorf("page=%d item[%d]=%q want=%q", page, i, item.ID, wantID)
			}
		}
		if page < 2 && got.NextAfter != got.Items[len(got.Items)-1].ID {
			t.Errorf("page=%d next_after=%v", page, got.NextAfter)
		}
		if page == 2 && got.NextAfter != nil {
			t.Errorf("final next_after=%v want null", got.NextAfter)
		}
	}

	exactFixture := apiReadFixture{sessions: []session.Session{{ID: "exact"}}}
	for i := 0; i < 100; i++ {
		exactFixture.messages = append(exactFixture.messages, message.Message{ID: fmt.Sprintf("x-%03d", i), SessionID: "exact", CreatedAt: int64(i)})
	}
	exactHandler := NewAPIHandler(APIRoutesWithDeps(exactFixture))
	exactFinal := apiReadRequest(t, exactHandler, "/api/v1/sessions/exact/messages?limit=50&after=x-049", []string{"read"})
	var exactResult struct {
		Items     []APIMessage `json:"items"`
		NextAfter any          `json:"next_after"`
	}
	if exactFinal.Code != http.StatusOK || decodeAPIResponse(exactFinal, &exactResult) != nil || len(exactResult.Items) != 50 || exactResult.NextAfter != nil {
		t.Fatalf("exact-multiple final page status=%d body=%s", exactFinal.Code, exactFinal.Body.String())
	}

	defaultPage := apiReadRequest(t, handler, "/api/v1/sessions/s/messages", []string{"read"})
	var defaultResult struct {
		Items []APIMessage `json:"items"`
	}
	if defaultPage.Code != http.StatusOK || decodeAPIResponse(defaultPage, &defaultResult) != nil || len(defaultResult.Items) != 50 {
		t.Fatalf("default page status=%d body=%s", defaultPage.Code, defaultPage.Body.String())
	}
	maxPage := apiReadRequest(t, handler, "/api/v1/sessions/s/messages?limit=200", []string{"read"})
	var maxResult struct {
		Items []APIMessage `json:"items"`
	}
	if maxPage.Code != http.StatusOK || decodeAPIResponse(maxPage, &maxResult) != nil || len(maxResult.Items) != 120 {
		t.Fatalf("maximum page status=%d body=%s", maxPage.Code, maxPage.Body.String())
	}
	for _, query := range []string{"?limit=201", "?limit=0", "?limit=abc", "?limit=1&limit=2"} {
		response := apiReadRequest(t, handler, "/api/v1/sessions/s/messages"+query, []string{"read"})
		if response.Code != http.StatusBadRequest {
			t.Errorf("query=%q status=%d body=%s", query, response.Code, response.Body.String())
		}
	}
	foreignCursor := apiReadRequest(t, handler, "/api/v1/sessions/s/messages?after=not-in-this-session", []string{"read"})
	if foreignCursor.Code != http.StatusBadRequest {
		t.Fatalf("foreign cursor status=%d body=%s", foreignCursor.Code, foreignCursor.Body.String())
	}
}

func TestAPIReadMessageToolPartsExactJSON(t *testing.T) {
	fixture := apiReadFixture{
		sessions: []session.Session{{ID: "s"}},
		messages: []message.Message{{
			ID: "m", SessionID: "s", Role: message.Assistant, CreatedAt: 1,
			Parts: []message.ContentPart{
				message.TextContent{Text: "hello"},
				message.ToolCall{ID: "call-1", Name: "lookup", Input: `{"q":"x"}`},
				message.ToolResult{ToolCallID: "call-1", Name: "lookup", Content: "found", IsError: true},
				message.ReasoningContent{Thinking: "private thought"},
			},
		}},
	}
	handler := NewAPIHandler(APIRoutesWithDeps(fixture))
	response := apiReadRequest(t, handler, "/api/v1/sessions/s/messages", []string{"read"})
	const want = "{\"items\":[{\"id\":\"m\",\"session_id\":\"s\",\"role\":\"assistant\",\"parts\":[{\"type\":\"text\",\"text\":\"hello\"},{\"type\":\"tool_call\",\"id\":\"call-1\",\"name\":\"lookup\",\"input\":\"{\\\"q\\\":\\\"x\\\"}\"},{\"type\":\"tool_result\",\"name\":\"lookup\",\"tool_call_id\":\"call-1\",\"content\":\"found\",\"is_error\":true}],\"created_at\":\"1970-01-01T00:00:01Z\"}],\"next_after\":null}\n"
	if response.Code != http.StatusOK || response.Body.String() != want {
		t.Fatalf("status=%d body=%s\nwant=%s", response.Code, response.Body.String(), want)
	}
}
