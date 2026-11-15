-- +goose Up
CREATE TABLE session_role_contracts (
    session_id TEXT PRIMARY KEY NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    contract_json TEXT NOT NULL
);

CREATE TABLE session_root_role_contracts (
    session_id TEXT PRIMARY KEY NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    logical_call_id TEXT NOT NULL,
    contract_json TEXT
);

-- +goose Down
DROP TABLE session_root_role_contracts;
DROP TABLE session_role_contracts;
