# Release Operations

## Conclusion

Release operations exist.

## Evidence Checked

- `task.json`
- `prd.md`
- `brief.md`
- `design.md`
- `implement.md`
- `implement.jsonl`
- `check.jsonl`
- `validation.md`
- Git commits `b76f930`, `f19fcdc`, and `b2454a2`
- Archived task `08-12-p2-stream-close-lifecycle/release.md`

## Drift Check

Missing `release.md`; this file records the deployment refresh, credential rotation, retained test resource cleanup, rollback, and post-release verification proven necessary by the real-environment validation.

## SQL Changes

None.

## Configuration Changes

- `[08-11-p2-live-validation]` Existing ark systemd services must be regenerated so both service units contain `CacheDirectory=ark`, `CacheDirectoryMode=0700`, and `XDG_CACHE_HOME=/var/cache/ark`.
- `[08-11-p2-live-validation]` No `ark.yaml` field changes are required.

## Batch / Deployment Scripts / Data Repair

- `[08-11-p2-live-validation]` After deploying the new ark binary, run `ark validate`, `ark doctor --all`, and `ark install` with the production manifest, then run `systemctl daemon-reload` before the next scheduled or manual backup.

## External Systems / Dependent Platforms

- `[08-11-p2-live-validation]` Rotate the root management password used during real-environment validation.
- `[08-11-p2-live-validation]` The Object Lock retention ended at `2026-08-13T04:31:01Z`; delete the residual Docker volume `ark-live-validation-minio-data` after confirming it is still the isolated validation volume. Do not use an Object Lock bypass or delete backend files directly.

## Release Order

1. Deploy the new ark binary.
2. Run `ark validate` and `ark doctor --all`.
3. Run `ark install` with the production manifest.
4. Run `systemctl daemon-reload`.
5. Start `ark-backup.service` manually once and verify the result before relying on timers.
6. Complete the management-password rotation and remove the retained isolated test volume.

## Rollback Notes

- Roll back the ark binary, rerun the older binary's `ark install`, and run `systemctl daemon-reload`.
- `/var/cache/ark` may remain after rollback; it contains restic cache data rather than backup source data or credentials.
- Do not bypass Object Lock or remove MinIO backend files to accelerate cleanup.

## Post-release Verification

- Run `systemd-analyze verify` against the generated ark service and timer units.
- Confirm `/var/cache/ark` is created as `root:root` with mode `0700`.
- Confirm `systemctl start ark-backup.service` finishes with `Result=success` and `ExecMainStatus=0`.
- Verify a failed target or manifest backup that returns a snapshot ID removes that exact snapshot from the repository.
- Confirm doctor failure summaries contain failed check names but not check `Detail` values.
- Confirm the management password has been rotated and `ark-live-validation-minio-data` has been removed.
