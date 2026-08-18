CREATE TABLE alert_states (
    id TEXT PRIMARY KEY,
    host TEXT NOT NULL CHECK (length(trim(host)) > 0),
    kind TEXT NOT NULL CHECK (
        kind IN ('backup_overdue', 'backup_consecutive_failures', 'verification_failed')
    ),
    active INTEGER NOT NULL CHECK (active IN (0, 1)),
    first_seen_at INTEGER NOT NULL CHECK (first_seen_at >= 0),
    last_seen_at INTEGER NOT NULL CHECK (last_seen_at >= first_seen_at),
    last_alert_sent_at INTEGER CHECK (
        last_alert_sent_at IS NULL OR last_alert_sent_at >= first_seen_at
    ),
    resolved_at INTEGER CHECK (
        resolved_at IS NULL OR resolved_at >= first_seen_at
    ),
    recovery_sent_at INTEGER CHECK (
        recovery_sent_at IS NULL OR recovery_sent_at >= resolved_at
    ),
    CHECK (
        (active = 1 AND resolved_at IS NULL AND recovery_sent_at IS NULL) OR
        (active = 0 AND resolved_at IS NOT NULL)
    )
);

CREATE INDEX idx_alert_states_active ON alert_states(active, host, kind);
