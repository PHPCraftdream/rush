-- +goose Up
CREATE TABLE mcp_outputs (
    id TEXT PRIMARY KEY NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    tool_name TEXT NOT NULL,
    tool_call_id TEXT NOT NULL,
    is_error INTEGER NOT NULL CHECK (is_error IN (0, 1)),
    total_bytes INTEGER NOT NULL CHECK (total_bytes >= 0 AND total_bytes <= 8388608)
);
CREATE INDEX mcp_outputs_session ON mcp_outputs(session_id);
CREATE TABLE mcp_output_chunks (
    output_id TEXT NOT NULL REFERENCES mcp_outputs(id) ON DELETE CASCADE,
    chunk_index INTEGER NOT NULL,
    data BLOB NOT NULL CHECK (length(data) <= 32768),
    PRIMARY KEY (output_id, chunk_index)
);

-- +goose Down
DROP TABLE mcp_output_chunks;
DROP TABLE mcp_outputs;
