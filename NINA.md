# Project guidelines

## Development

- Linux-only storage software: prefer simple, explicit, fail-closed state machines.
- The compiled `cmd/mirror` binary is the only user-facing program. Add subcommands, not entrypoints; keep shared behavior in normal packages. Build with `make`.
- Repository automation belongs in `bin/*.go`, uses only the Go standard library, and runs with `go run`. Shell is limited to thin launchers locating the repository and invoking Go or `./mirror`; no storage logic, parsing, or pipelines.
- Run `go run ./bin/check.go` after code changes. Tests must exercise production behavior. Fast tests inject OS boundaries; never expose host block devices to destructive tools. Real storage tests use disposable images only inside an isolated VM.
- Make hard cutovers, not compatibility paths. Command behavior and runtime formats are documented in [readme.md](readme.md).

## Safety boundaries

- Destructive work requires complete preflight, explicit confirmation, and immediate identity/state revalidation. Formatting accepts only whole `/dev/sd[a-z]+` disks and probes required tools without devices or mutations. Never install dependencies automatically.
- `format`, `mount`, and `unmount` must be launched through sudo, never invoke it internally, and use the validated caller's home and ownership. Repair approval comes from the controlling terminal, not piped stdin.
- Every disk-operation CLI handler, including previews, holds the caller-home `.mirror/operation.lock` through work, diagnostics, and cleanup. Never replace/unlink its inode. External/manual writers must stay off mirror disks; this lock is advisory coordination.
- **Manual recovery for format/mount/unmount:** never automatically unmount, close mappers, or roll back. Retain private action logs and read-only failure diagnostics. Sync intent before actions; failed commands may already have changed state. Invalid/unavailable observations are unknown. Never log passwords, stdin, or external action-output streams.
- Copy/repair relocking remains automatic and independently bounded, including after cancellation or publication failure. Planning, publication, and guards share retained root descriptors; identity loss stops publication. Cleanup touches only original write destinations, never reopened replacement paths. Ensure leaves source-only directory modes unchanged.
- Confine filesystem access with Linux `openat2`: no symlink traversal or nested mounts. Reject unsafe configuration before inventory. Share inventory decoding through `internal/inventory`; preserve duplicate/stacked evidence and opaque sources, leaving eligibility decisions to command packages.
- External storage commands use `system.Exec`/`RunCommand` with exact argv, not shell logic. Keep owned process groups contained through cancellation and helper cleanup before releasing the operation lock; preserve terminal ownership/modes. This supports trusted non-daemonizing tools, not adversarial containment.
- Propagate context and plan, prompt, progress, status, and cleanup errors. Failed plan/prompt output stops before confirmation input or mutation. A failed status write does not undo an already completed action.
- Unmount/close must accept read-only devices and unknown/non-ext4 decrypted filesystems; mounting still requires writable devices and ext4. Do not weaken identity, LUKS/mapper relationship, holder, mount-conflict, or confirmation checks for deactivation.

## Product choices

- Mirror immutable regular files and symlinks. **No empty-directory replication**, including previews; create only needed parents and do not delete existing empty directories.
- Symlink targets are opaque: preserve relative, absolute, dangling, and looping targets exactly, without normalization or traversal. Reject disagreements and source drift; copying a link never copies its external target data.
- `.b2sum` is metadata, never a regular payload or symlink; directory names may use it. Reject sidecars of sidecars/non-regular entries. Preserve orphan sidecar evidence, including malformed bytes without votes; never infer payload intent from contents.
- Ensure preflights every missing data/sidecar pair before unlocking. Any existing sidecar—even matching—blocks publication for manual resolution. Recheck at publication and retain no-replace guards. Refuse checksum-only paths with no regular data source.
- Repair scans are observational, not ingest. Each configured mirror contributes at most once per candidate; data and valid sidecar may support different candidates. **Exactly one candidate must exceed half of all configured mirrors**, with matching actual source data. Absent mirrors never reduce the denominator; missing/malformed sidecars do not vote.
- Retain unresolved findings as historical evidence, never apply targets. Confirm and repair only the independent safe subset. Operational/checkpoint failures stop later work; remaining issues return nonzero. After manual resolution, require a fresh scan.
- Keep scan evidence immutable and checkpoint progress with small synced records. Persist starts before publication and completion only after desired-state verification. Resume revalidates mounts and every safe path: completed files require desired bytes, unfinished files allow scanned or desired bytes, omitted copies remain absent, and sidecar-only evidence retains absent data and exact metadata bytes.
- `.mirror-internal/` is a marked private namespace excluded from scanning, replication, and chmod walks (as is `lost+found`). Keep publication temporaries and quarantined corruption separate from payloads. Reject unmarked collisions; never automatically delete evidence or migrate legacy backups.
