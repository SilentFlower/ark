# Release Operations

## Conclusion

Release operations exist.

## Evidence Checked

- `task.json`
- `prd.md`
- `design.md`
- `implement.md`
- `implement.jsonl`
- `check.jsonl`
- Git commit `ae0eae3 fix: 兼容 Redis readiness 多行输出`

## Drift Check

Missing `release.md`; deployment and post-release verification requirements are explicit in the task artifacts.

## SQL Changes

None.

## Configuration Changes

None. Do not change Redis passwords, ACLs, environment variables, or Compose configuration for this release.

## Batch / Deployment Scripts / Data Repair

- On hub, record commit `ae0eae3`, the static binary SHA-256, and the current `biz` production baseline.
- Create a new timestamped backup of `/usr/local/bin/ark` before replacing it.
- Atomically deploy the statically linked Ark binary built with `CGO_ENABLED=0`.
- Do not execute data repair or modify production Redis data as part of this release.

## External Systems / Dependent Platforms

- Production execution requires operator access to hub and the configured `biz` destination.
- Run the existing dnsmgr AuthApi check; no dnsmgr configuration change is included.

## Release Order

1. Capture the current binary checksum and production project/container/network/volume/files baseline.
2. Create and verify the timestamped binary backup on hub.
3. Atomically deploy the new binary.
4. Run `ark validate`, the full doctor check, and the dnsmgr AuthApi check.
5. Run `ark verify --host biz --snapshot latest --json` and retain the structured result.
6. Verify production baselines, isolation cleanup, and verify service/timer state.

## Rollback Notes

1. Stop the new manual verify run.
2. Restore the timestamped pre-deployment binary backup and verify its SHA-256.
3. Rerun validate, doctor, and timer checks.
4. Clean up only resources proven to belong to the run by its isolation ID and structured cleanup command.
5. Do not delete resources whose ownership cannot be proven.

## Post-release Verification

- Redis readiness accepts a successful `redis-cli PING` result containing warnings plus an independent `PONG` line without entering another poll.
- `ark verify --host biz --snapshot latest --json` completes successfully.
- The rehearsal container is not connected to production `api_shared`.
- Production project/container/network/volume/files baselines are unchanged.
- No isolation container, network, volume, or restore root remains after cleanup.
- The verify service and timer remain healthy.
