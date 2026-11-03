package session

import "context"

// BatchAPIRoot is one root batch of an API service session.
type BatchAPIRoot struct{ Root, State, Ref string }

// BatchAPIRoots lists the root batches an owner session holds, oldest first.
// Ref is the submit reference the root was admitted under; the API encodes its
// Idempotency-Key there, so the listing is the durable idempotency index.
func (s *BatchStore) BatchAPIRoots(ctx context.Context, owner string) ([]BatchAPIRoot, error) {
	rows, err := s.writer.QueryContext(ctx, `SELECT root_id,state,COALESCE(tool_call_id,'') FROM batch_nodes WHERE owner_session_id=? AND node_id=root_id ORDER BY created_at,root_id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BatchAPIRoot{}
	for rows.Next() {
		var x BatchAPIRoot
		if err = rows.Scan(&x.Root, &x.State, &x.Ref); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// BatchAPIRootLive reports whether a root state still occupies a slot of the
// token's live-root budget (pending, starting, running and cancelling count).
func BatchAPIRootLive(state string) bool {
	switch state {
	case "pending", "starting", "running", "cancelling":
		return true
	}
	return false
}
