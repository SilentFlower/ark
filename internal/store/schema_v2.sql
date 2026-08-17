CREATE TABLE manual_operations (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (
        kind IN ('backup', 'verify', 'restore_preview', 'restore')
    ),
    host TEXT NOT NULL CHECK (length(trim(host)) > 0),
    status TEXT NOT NULL CHECK (
        status IN ('running', 'ok', 'fail', 'interrupted')
    ),
    started_at INTEGER NOT NULL CHECK (started_at >= 0),
    finished_at INTEGER CHECK (finished_at IS NULL OR finished_at >= started_at),
    duration_ms INTEGER CHECK (duration_ms IS NULL OR duration_ms >= 0),
    request_json TEXT NOT NULL CHECK (json_valid(request_json)),
    result_json TEXT CHECK (result_json IS NULL OR json_valid(result_json)),
    error TEXT NOT NULL DEFAULT '',
    exit_code INTEGER,
    parent_operation_id TEXT,
    CHECK (
        (status = 'running' AND finished_at IS NULL AND duration_ms IS NULL) OR
        (status != 'running' AND finished_at IS NOT NULL AND duration_ms IS NOT NULL)
    ),
    FOREIGN KEY (parent_operation_id) REFERENCES manual_operations(id) ON DELETE SET NULL
);

CREATE INDEX idx_manual_operations_started
    ON manual_operations(started_at DESC, id DESC);
CREATE INDEX idx_manual_operations_host_started
    ON manual_operations(host, started_at DESC, id DESC);
CREATE INDEX idx_manual_operations_status_started
    ON manual_operations(status, started_at DESC, id DESC);
