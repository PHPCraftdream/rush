package server

// WS-triggered graceful shutdown of the whole server process (task #714).
// The web UI's "Shut down server" control sends shutdown_server; this
// handler acks and then converges on the exact shutdown path a SIGINT
// takes (Start's watcher -> srv.Shutdown -> runWebMode's deferred
// app.Shutdown). There is deliberately no parallel teardown here.

import "log/slog"

// handleShutdownServer replies success FIRST — the client must receive a
// clean ack before the shutdown sequence tears the listener (and soon the
// process) down — then signals Start()'s shutdown watcher. The reply is a
// non-blocking enqueue into the client's buffered send channel, and
// nothing closes the connection before the write pump has flushed it, so
// ordering is safe in practice.
func handleShutdownServer(s *Server, c *Client, msg WSMessage) {
	c.reply(msg.ID, EventResponse, map[string]string{"status": "shutting_down"}, "")
	slog.Info("ws: shutdown_server requested by client")
	s.requestShutdown()
}
