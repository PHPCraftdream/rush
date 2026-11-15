-- +goose Up
-- W3 wake_only check-in receipts: one-shot marker per exact batch node
-- incarnation (root, node, host, token) and check-in kind. Committed
-- ATOMICALLY with its session_notices row by the App-owned
-- BatchTimeoutCheckin scanner; the receipt survives notice delivery,
-- reaction and retention while the node exists, so a re-scan (or a service
-- restart, or notice retention deleting the notice row) can never repeat a
-- check-in. Cascade-deleted with the batch node it receipts.
-- +goose StatementBegin
CREATE TABLE batch_timeout_checkins (
    root_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    host_id TEXT NOT NULL,
    token INTEGER NOT NULL CHECK (token >= 0),
    kind TEXT NOT NULL,
    owner_session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (root_id, node_id, host_id, token, kind),
    FOREIGN KEY (root_id, node_id, owner_session_id, host_id)
        REFERENCES batch_nodes (root_id, node_id, owner_session_id, host_id) ON DELETE CASCADE
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE batch_timeout_checkins;
-- +goose StatementEnd
