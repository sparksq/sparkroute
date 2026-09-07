CREATE TABLE IF NOT EXISTS gateway_config_managed_sets (
    owner TEXT PRIMARY KEY,
    revision_id TEXT NOT NULL,
    document_json TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    updated_by TEXT NOT NULL,
    reason TEXT,
    CHECK (owner IN ('operator', 'sparkrun')),
    CHECK (length(revision_id) = 64),
    CHECK (length(updated_by) BETWEEN 1 AND 512),
    CHECK (reason IS NULL OR length(reason) <= 4096),
    CHECK (json_valid(document_json))
);

CREATE TABLE IF NOT EXISTS gateway_config_current (
	singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
	revision_id TEXT NOT NULL,
	document_json TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	updated_by TEXT NOT NULL,
	reason TEXT,
	CHECK (length(revision_id) = 64),
	CHECK (length(updated_by) BETWEEN 1 AND 512),
	CHECK (reason IS NULL OR length(reason) <= 4096),
	CHECK (json_valid(document_json))
);
