package server

import (
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
)

func websocketUpgrade(r *http.Request) bool {
	return websocket.IsWebSocketUpgrade(r)
}

func hasBearerSubprotocol(r *http.Request) bool {
	for _, value := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, item := range strings.Split(value, ",") {
			if strings.HasPrefix(strings.TrimSpace(item), "rush.bearer.") {
				return true
			}
		}
	}
	return false
}

func offeredSubprotocol(r *http.Request, protocol string) bool {
	for _, value := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, item := range strings.Split(value, ",") {
			if strings.TrimSpace(item) == protocol {
				return true
			}
		}
	}
	return false
}
