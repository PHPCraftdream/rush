package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
)

// Retained-text logical-byte quotas, not physical SQLite/RSS or provider limits.
// Metadata, database pages and WAL overhead are additional. Exhaustion rejects
// retention explicitly after execution; it never evicts a replay reference.
const (
	MCPOutputLimit        = 8 << 20
	MCPOutputSessionLimit = 64 << 20
	MCPOutputStoreLimit   = 256 << 20
	mcpOutputChunkBytes   = 32 << 10
)

var ErrMCPOutputQuota = errors.New("MCP output retention quota exceeded")

// MCPOutputTransaction must own trusted admission, begin/validate the transaction,
// run effect, commit/rollback, and release admission. Read callbacks must hold
// trusted read authority for their snapshot. Nil is blocked, never unrestricted.
// The finalization-aware service adapter is deliberately not installed here.
type MCPOutputTransaction func(context.Context, string, func(*sql.Tx) error) error

type MCPOutputStore struct {
	mutate MCPOutputTransaction
	read   MCPOutputTransaction
}

// NewMCPOutputStore requires finalization-aware transaction owners for activation.
func NewMCPOutputStore(mutate, read MCPOutputTransaction) *MCPOutputStore {
	return &MCPOutputStore{mutate: mutate, read: read}
}

// RetainMCPOutput commits a complete immutable artifact or nothing. The caller
// must hold its trusted producer admission across this operation. Quotas reject
// new artifacts; referenced artifacts never expire or get evicted behind replay.
// Session deletion cascades through both tables, including after a restart.
func (s *MCPOutputStore) RetainMCPOutput(ctx context.Context, owner, tool, callID string, isError bool, total int64, render func(io.Writer) error) (string, error) {
	if s == nil || s.mutate == nil || s.read == nil {
		return "", errors.New("MCP output retention activation is blocked: both trusted transaction owners are required")
	}
	var id string
	err := s.mutate(ctx, owner, func(tx *sql.Tx) error {
		var err error
		id, err = retainMCPOutputTx(ctx, tx, owner, tool, callID, isError, total, render)
		return err
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

func retainMCPOutputTx(ctx context.Context, tx *sql.Tx, owner, tool, callID string, isError bool, total int64, render func(io.Writer) error) (string, error) {
	if owner == "" || tool == "" || callID == "" || render == nil {
		return "", errors.New("MCP output retention requires an admitted owner, source tool and call")
	}
	if total < 0 || total > MCPOutputLimit {
		return "", ErrMCPOutputQuota
	}
	id := uuid.NewString()
	// The first statement is a write: concurrent writers cannot reserve quota
	// from the same stale read snapshot. Failure/busy/cancellation rolls back.
	result, err := tx.ExecContext(ctx, `INSERT INTO mcp_outputs
		(id, session_id, tool_name, tool_call_id, is_error, total_bytes)
		SELECT ?, ?, ?, ?, ?, ? WHERE
		COALESCE((SELECT SUM(total_bytes) FROM mcp_outputs WHERE session_id = ?), 0) + ? <= ?
		AND COALESCE((SELECT SUM(total_bytes) FROM mcp_outputs), 0) + ? <= ?`,
		id, owner, tool, callID, isError, total, owner, total, MCPOutputSessionLimit, total, MCPOutputStoreLimit)
	if err != nil {
		return "", err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if n != 1 {
		return "", ErrMCPOutputQuota
	}
	writer := &mcpOutputWriter{ctx: ctx, tx: tx, id: id, expected: total, buffer: make([]byte, 0, mcpOutputChunkBytes)}
	if err := render(writer); err != nil {
		return "", err
	}
	if writer.written != total {
		return "", fmt.Errorf("MCP output capture size changed: expected %d, captured %d", total, writer.written)
	}
	if err := writer.flush(); err != nil {
		return "", err
	}
	return id, nil
}

type mcpOutputWriter struct {
	ctx               context.Context
	tx                *sql.Tx
	id                string
	expected, written int64
	index             int
	buffer            []byte
}

func (w *mcpOutputWriter) WriteString(s string) (int, error) {
	total := 0
	for len(s) > 0 {
		take := min(len(s), mcpOutputChunkBytes)
		n, err := w.Write([]byte(s[:take]))
		total += n
		if err != nil {
			return total, err
		}
		s = s[take:]
	}
	return total, nil
}

func (w *mcpOutputWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.expected-w.written {
		return 0, errors.New("MCP output capture exceeded reserved size")
	}
	n := len(p)
	for len(p) > 0 {
		take := min(len(p), mcpOutputChunkBytes-len(w.buffer))
		w.buffer = append(w.buffer, p[:take]...)
		w.written += int64(take)
		p = p[take:]
		if len(w.buffer) == mcpOutputChunkBytes {
			if err := w.flush(); err != nil {
				return n - len(p), err
			}
		}
	}
	return n, nil
}

func (w *mcpOutputWriter) flush() error {
	if len(w.buffer) == 0 {
		return nil
	}
	if _, err := w.tx.ExecContext(w.ctx, `INSERT INTO mcp_output_chunks (output_id, chunk_index, data) VALUES (?, ?, ?)`, w.id, w.index, w.buffer); err != nil {
		return err
	}
	w.index++
	w.buffer = w.buffer[:0]
	return nil
}

// ReadMCPOutput returns one bounded byte page under a single read snapshot.
// This is storage isolation, not permission authority: the resource tool must
// additionally enforce its own permission and the source tool's current grant.
func (s *MCPOutputStore) ReadMCPOutput(ctx context.Context, owner, id string, offset int64, limit int) (tool string, isError bool, total int64, data []byte, err error) {
	if s == nil || s.mutate == nil || s.read == nil {
		err = errors.New("MCP output read activation is blocked: both trusted transaction owners are required")
		return
	}
	err = s.read(ctx, owner, func(tx *sql.Tx) error {
		var readErr error
		tool, isError, total, data, readErr = readMCPOutputTx(ctx, tx, owner, id, offset, limit)
		return readErr
	})
	if err != nil {
		return "", false, 0, nil, err
	}
	return
}

func readMCPOutputTx(ctx context.Context, tx *sql.Tx, owner, id string, offset int64, limit int) (tool string, isError bool, total int64, data []byte, err error) {
	if offset < 0 || limit <= 0 || limit > mcpOutputChunkBytes {
		err = errors.New("invalid MCP output byte page")
		return
	}
	err = tx.QueryRowContext(ctx, `SELECT tool_name, is_error, total_bytes FROM mcp_outputs WHERE session_id = ? AND id = ?`, owner, id).Scan(&tool, &isError, &total)
	if err != nil {
		return
	}
	if offset > total {
		err = errors.New("MCP output byte offset exceeds retained output")
		return
	}
	end := min(total, offset+int64(limit))
	if offset == end {
		return tool, isError, total, []byte{}, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT chunk_index, data FROM mcp_output_chunks WHERE output_id = ? AND chunk_index BETWEEN ? AND ? ORDER BY chunk_index`, id, offset/mcpOutputChunkBytes, (end-1)/mcpOutputChunkBytes)
	if err != nil {
		return
	}
	defer rows.Close()
	data = make([]byte, 0, end-offset)
	for rows.Next() {
		var index int64
		var chunk []byte
		if err = rows.Scan(&index, &chunk); err != nil {
			return
		}
		base := index * mcpOutputChunkBytes
		start, stop := max(offset-base, 0), min(end-base, int64(len(chunk)))
		if start > stop || stop < 0 || start > int64(len(chunk)) {
			err = errors.New("invalid MCP output chunk")
			return
		}
		data = append(data, chunk[start:stop]...)
	}
	if err = rows.Err(); err != nil {
		return
	}
	if int64(len(data)) != end-offset {
		err = errors.New("incomplete retained MCP output")
	}
	return
}
