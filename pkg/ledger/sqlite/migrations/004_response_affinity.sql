CREATE TABLE IF NOT EXISTS llm_response_affinities (
    owner_scope TEXT NOT NULL
		CHECK (length(owner_scope) BETWEEN 1 AND 128),
    response_id TEXT NOT NULL
		CHECK (length(response_id) BETWEEN 1 AND 512),
    virtual_model TEXT NOT NULL,
    provider TEXT NOT NULL,
    deployment TEXT NOT NULL,
    upstream_model TEXT NOT NULL,
    bound_at TEXT NOT NULL,
    bound_at_unix_ns INTEGER NOT NULL,
    expires_at TEXT NOT NULL,
	expires_at_unix_ns INTEGER NOT NULL,
	PRIMARY KEY (owner_scope, response_id)
);

CREATE INDEX IF NOT EXISTS llm_response_affinities_expiry_idx
    ON llm_response_affinities (expires_at_unix_ns);
