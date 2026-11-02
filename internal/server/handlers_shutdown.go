package server

// WS-triggered graceful shutdown of the whole server process (task #714).
// The web UI's "Shut down server" control sends shutdown_server; this
// handler acks and then converges on the exact shutdown path a SIGINT
// takes (Start's watcher -> srv.Shutdown -> runWebMode's deferred
// app.Shutdown). There is deliberately no parallel teardown here.

import "log/slog"

// handleShutdownServer waits until its acknowledgement is written to the
// WebSocket (or the bounded wait ends) before signaling the shutdown watcher.
// This keeps teardown from closing the connection ahead of the queued reply.
func handleShutdownServer(s *Server, c *Client, msg WSMessage) {
	c.replyAndWait(msg.ID, EventResponse, map[string]string{"status": "shutting_down"}, "", shutdownAckWriteLimit)
	slog.Info("ws: shutdown_server requested by client")
	s.requestShutdown()
}
