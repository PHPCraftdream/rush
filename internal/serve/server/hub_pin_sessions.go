package server

import "github.com/PHPCraftdream/rush/internal/session"

// filterSessionsForPinnedClient gives list_sessions the same pinned-session
// visibility boundary as event delivery. Unbound clients retain the full list.
func filterSessionsForPinnedClient(sessions []session.Session, c *Client) []session.Session {
	if c == nil || c.auth == nil || c.auth.view.sessionID == "" {
		return sessions
	}
	return pinnedSessionsOnly(sessions, c.auth.view.sessionID)
}

// pinnedSessionsOnly keeps only the pinned session of a list.
func pinnedSessionsOnly(sessions []session.Session, pin string) []session.Session {
	kept := make([]session.Session, 0, 1)
	for _, sess := range sessions {
		if sess.ID == pin {
			kept = append(kept, sess)
		}
	}
	return kept
}
