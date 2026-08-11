CREATE TABLE runs (
    id TEXT PRIMARY KEY,
    requested_host TEXT,
    status TEXT NOT NULL CHECK (status IN ('running', 'ok', 'warn', 'fail')),
    started_at INTEGER NOT NULL CHECK (started_at >= 0),
    finished_at INTEGER CHECK (finished_at IS NULL OR finished_at >= started_at),
    duration_ms INTEGER CHECK (duration_ms IS NULL OR duration_ms >= 0),
    ark_version TEXT NOT NULL CHECK (length(trim(ark_version)) > 0),
    error TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_runs_started_at ON runs(started_at DESC);
CREATE INDEX idx_runs_status_started_at ON runs(status, started_at DESC);

CREATE TABLE run_targets (
    run_id TEXT NOT NULL,
    host TEXT NOT NULL CHECK (length(trim(host)) > 0),
    target_id TEXT NOT NULL CHECK (length(trim(target_id)) > 0),
    target_type TEXT NOT NULL CHECK (length(trim(target_type)) > 0),
    status TEXT NOT NULL CHECK (status IN ('ok', 'warn', 'fail')),
    bytes INTEGER NOT NULL CHECK (bytes >= 0),
    duration_ms INTEGER NOT NULL CHECK (duration_ms >= 0),
    snapshot_id TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, host, target_id),
    FOREIGN KEY (run_id) REFERENCES runs(id) ON DELETE CASCADE
);

CREATE INDEX idx_run_targets_lookup
    ON run_targets(host, target_id, status, run_id);

CREATE TABLE doctor_reports (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    scope TEXT NOT NULL CHECK (scope IN ('local', 'host')),
    host TEXT,
    created_at INTEGER NOT NULL CHECK (created_at >= 0),
    status TEXT NOT NULL CHECK (status IN ('ok', 'warn', 'fail')),
    next_run_at INTEGER CHECK (next_run_at IS NULL OR next_run_at >= 0),
    report_json TEXT NOT NULL CHECK (json_valid(report_json)),
    CHECK (
        (scope = 'local' AND host IS NULL) OR
        (scope = 'host' AND host IS NOT NULL AND length(trim(host)) > 0)
    )
);

CREATE INDEX idx_doctor_reports_scope_host_created
    ON doctor_reports(scope, host, created_at DESC);

CREATE TABLE verifications (
    id TEXT PRIMARY KEY,
    host TEXT NOT NULL CHECK (length(trim(host)) > 0),
    run_id TEXT,
    snapshot_id TEXT NOT NULL CHECK (length(trim(snapshot_id)) > 0),
    started_at INTEGER NOT NULL CHECK (started_at >= 0),
    finished_at INTEGER NOT NULL CHECK (finished_at >= started_at),
    duration_ms INTEGER NOT NULL CHECK (duration_ms >= 0),
    status TEXT NOT NULL CHECK (status IN ('ok', 'warn', 'fail')),
    error TEXT NOT NULL DEFAULT '',
    detail_json TEXT NOT NULL CHECK (json_valid(detail_json)),
    FOREIGN KEY (run_id) REFERENCES runs(id) ON DELETE SET NULL
);

CREATE INDEX idx_verifications_host_started
    ON verifications(host, started_at DESC);
