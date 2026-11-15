package server

import "net/http"

func apiEventsRoute() APIRoute {
	return APIRoute{
		Method: http.MethodGet, Pattern: "/sessions/{id}/events", Scope: "read",
		Summary: "Stream session events", OperationID: "streamSessionEvents",
		Handler: apiReadPlaceholder,
		Params: []APIParam{
			{Name: "id", In: "path", Required: true, Description: "Session ID"},
			{Name: "Last-Event-ID", In: "header", Description: "Resume after event ID"},
			{Name: "last_event_id", In: "query", Description: "Resume after event ID"},
		},
		ResponseDescription: "Session event stream",
	}
}
