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
- Git commits `2ff5a01` and `8a17ed9`

## Drift Check

Missing `release.md`; this file records the configuration compatibility and verification steps found in the task and Git evidence.

## SQL Changes

None.

## Configuration Changes

- `[08-12-p2-ssh-host-key-usability]` Existing schema v2 manifests remain valid without edits, but an omitted `ssh.host_key_policy` now means `accept-new` instead of the previous strict behavior.
- `[08-12-p2-ssh-host-key-usability]` Environments that must preserve pre-established trust must add `host_key_policy: strict` before the next unattended backup or doctor run.
- `[08-12-p2-ssh-host-key-usability]` `known_hosts_file` remains required and must be an absolute path. For `accept-new`, its parent directory must already exist and be writable and searchable by the ark process.

## Batch / Deployment Scripts / Data Repair

None.

## External Systems / Dependent Platforms

- `[08-12-p2-ssh-host-key-usability]` When a host key has changed, an administrator must verify the scanned SHA256 fingerprint through the cloud console, server-local terminal, or another independent channel before running `ark host-key refresh --host <name> --apply`.

## Release Order

1. Decide whether each remote host should use the new default `accept-new` or explicit `strict` behavior.
2. Update and validate manifests before the next unattended execution when explicit `strict` is required.
3. Deploy the new ark binary.
4. Run `ark validate`, then `ark doctor`; use the explicit refresh workflow only for verified host-key changes.

## Rollback Notes

- Roll back the binary and manifest together.
- Before starting an older ark binary, remove every `host_key_policy` field because strict YAML decoding in the previous version rejects that unknown field.
- Existing `known_hosts_file` contents remain compatible; do not delete the whole trust store during rollback.

## Post-release Verification

- Verify an omitted policy uses `StrictHostKeyChecking=accept-new` and explicit `strict` uses `StrictHostKeyChecking=yes`.
- Verify first connection can create a record only under `accept-new` with a usable parent directory.
- Verify a changed recorded key is still rejected and the error points to `ark host-key refresh --host <name>`.
- Verify refresh preview performs zero writes and `--apply` changes only the selected host record after independent fingerprint confirmation.
