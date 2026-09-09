CREATE TABLE IF NOT EXISTS gateway_config_presets (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL COLLATE NOCASE UNIQUE,
    revision_id TEXT NOT NULL CHECK (length(revision_id) = 64),
    document_json TEXT NOT NULL CHECK (json_valid(document_json)),
    updated_at TEXT NOT NULL,
    CHECK (length(name) BETWEEN 1 AND 64)
);

CREATE TABLE IF NOT EXISTS gateway_config_preset_state (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    active_preset TEXT NOT NULL REFERENCES gateway_config_presets(id),
    revision INTEGER NOT NULL CHECK (revision >= 1)
);
