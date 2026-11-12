-- +goose Up
CREATE TABLE session_agent_finalizations (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    logical_call_id TEXT NOT NULL CHECK (logical_call_id <> ''),
    generation INTEGER NOT NULL CHECK (generation > 0),
    revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
    phase TEXT NOT NULL DEFAULT 'executing' CHECK (phase IN ('executing', 'waiting_work', 'composing', 'accepted', 'failed', 'cancelled', 'interrupted')),
    node_id TEXT NOT NULL DEFAULT '',
    node_token INTEGER NOT NULL DEFAULT 0 CHECK (node_token >= 0),
    candidate_message_id TEXT NOT NULL DEFAULT '',
    candidate_hash TEXT NOT NULL DEFAULT '',
    CHECK ((node_id = '' AND node_token = 0) OR (node_id <> '' AND node_token > 0)),
    PRIMARY KEY (session_id, logical_call_id, generation)
);

CREATE UNIQUE INDEX session_agent_finalizations_active_owner
ON session_agent_finalizations(session_id)
WHERE phase IN ('executing', 'waiting_work', 'composing');

CREATE TABLE session_agent_finalization_tickets (
    session_id TEXT NOT NULL,
    logical_call_id TEXT NOT NULL,
    generation INTEGER NOT NULL,
    ticket_id TEXT NOT NULL CHECK (ticket_id <> ''),
    released INTEGER NOT NULL DEFAULT 0 CHECK (released IN (0, 1)),
    PRIMARY KEY (session_id, logical_call_id, generation, ticket_id),
    FOREIGN KEY (session_id, logical_call_id, generation)
        REFERENCES session_agent_finalizations(session_id, logical_call_id, generation) ON DELETE CASCADE
);

CREATE INDEX session_agent_finalization_tickets_open
ON session_agent_finalization_tickets(session_id, logical_call_id, generation)
WHERE released = 0;

-- +goose Down
DROP TABLE session_agent_finalization_tickets;
DROP TABLE session_agent_finalizations;
