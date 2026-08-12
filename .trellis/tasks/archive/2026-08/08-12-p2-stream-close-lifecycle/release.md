# Release Operations

## Conclusion

Release operations exist.

## Evidence Checked

- `task.json`
- `prd.md`
- `brief.md`
- `implement.jsonl`
- `check.jsonl`
- Git commits `b76f930` and `3f84631`
- `internal/systemd/unit.go` and `internal/cli/install.go`
- P2 real-environment validation record

## Drift Check

Missing `release.md`; this file records the systemd refresh and external cleanup steps required by the implemented changes and validation evidence.

## SQL Changes

None.

## Configuration Changes

- `[08-12-p2-stream-close-lifecycle]` Existing ark systemd services must be regenerated so both service units contain `CacheDirectory=ark`, `CacheDirectoryMode=0700`, and `XDG_CACHE_HOME=/var/cache/ark`.
- `[08-12-p2-stream-close-lifecycle]` No `ark.yaml` field changes are required.

## Batch / Deployment Scripts / Data Repair

- `[08-12-p2-stream-close-lifecycle]` After deploying the new ark binary, run `ark install` with the production manifest, then run `systemctl daemon-reload` before the next scheduled or manual backup.

## External Systems / Dependent Platforms

- `[08-12-p2-stream-close-lifecycle]` Rotate the root management password used during real-environment validation.
- `[08-12-p2-stream-close-lifecycle]` After `2026-08-13T04:31:01Z`, delete the residual Docker volume `ark-live-validation-minio-data`; before that time its remaining sentinel version is protected by Object Lock and must not be bypassed.

## Release Order

1. Deploy the new ark binary.
2. Run `ark validate` and `ark doctor --all`.
3. Run `ark install` with the production manifest.
4. Run `systemctl daemon-reload`.
5. Start `ark-backup.service` manually once and verify the result before relying on timers.
6. Complete the management-password rotation and delete the retained test volume after its Object Lock retention expires.

## Rollback Notes

- Roll back the ark binary, rerun the older binary's `ark install`, and run `systemctl daemon-reload`.
- `/var/cache/ark` may remain after rollback; it contains restic cache data rather than backup source data or credentials.
- Do not bypass Object Lock to accelerate test-volume cleanup.

## Post-release Verification

- Run `systemd-analyze verify` against the generated ark service and timer units.
- Confirm `/var/cache/ark` is created as `root:root` with mode `0700`.
- Confirm `systemctl start ark-backup.service` finishes with `Result=success` and `ExecMainStatus=0`.
- Inject or simulate a backup failure that returns a snapshot ID and verify the exact target or manifest snapshot is no longer listed afterward.
- Confirm doctor failure summaries contain failed check names but not check `Detail` values.
