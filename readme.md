# Mirror

## Why

S3 is the safest place to store immutable data, but it's expensive to fetch that data due to egress bandwidth fees of $0.10/GB.

We need something local to mirror that S3 data, that is equally safe, and free to access.

ZFS is too complex. RAID doesn't protect against bitrot. We need something simpler. Something unborkable.

## How

On a Linux system, several spinning rust disks form a local mirror using ext4 and cryptsetup.

Write immutable data to any of them, then mirror it to all of them.

Periodically run repair, which detects bitrot and repairs the data from any other disk that has an uncorrupted copy.

Use a minimum of 2 disks. 3 is better. 4+ is fine for extreme durability.

## Build

Build the single `mirror` binary after changing or cloning the repository:

```bash
mirror-build
```

`bin/mirror` is a thin launcher for the resulting repository-root binary. Both scripts are available when this repository's `bin/` directory is on `PATH`.

## Commands

```text
mirror format
mirror mount
mirror unmount
mirror ensure-copies [-p|--preview]
mirror repair-copies [-p|--preview] [--resume]
mirror lock
mirror unlock
```

`mirror format` interactively selects a whole `/dev/sdX` disk, verifies that it is inactive, then creates a GPT, LUKS2 container, and ext4 filesystem.

`mirror mount` resolves the GPT disk identifiers in `~/.mirror/disks`, unlocks available disks, and mounts them. `mirror unmount` unmounts and closes configured disks, including recovery of an open but unmounted mapper.

Formatting, mounting, and unmounting require root. Launch the complete command through sudo; the binary does not invoke sudo internally:

```bash
sudo "$(command -v mirror)" mount
sudo "$(command -v mirror)" unmount
sudo "$(command -v mirror)" format
```

The binary validates sudo's caller identity against the system account database and continues to use the invoking user's home directory, UID, and GID. Direct root invocation without sudo caller metadata is rejected. Help never requires privilege, so `mirror unmount -h` works directly.

Format, mount, and unmount retain private JSONL action logs under `~/.mirror/operations/`; each invocation prints its unique log path and preserves earlier logs. Every command or mountpoint-directory creation has a synced `started` record before execution and a `succeeded` or `failed/uncertain` result. A process killed before recording its result leaves an uncertain started action—not evidence that nothing changed. Command success describes the command's exit status, not a guarantee about later disk state. Passwords/stdin and external command output streams are not copied into these logs. Structured inventory/verification observations are retained separately, including disk identifiers, serials, mapper relationships, filesystem types, and mountpoints.

These three commands **do not automatically unmount, close mappers, or undo changes after a failure**. They attempt read-only `lsblk` and `findmnt` diagnostics with an independent 30-second context, retain those snapshots in the log, and mark unavailable/invalid observations `unknown`. A cancelled or failed action may already have taken effect; mounts and decrypted mappers may remain active. Inspect the log (for example, `jq . LOG_PATH`) and revalidate the current disk identity before manual intervention. Do not rerun `format` merely to retry opening or mounting: formatting is destructive. Use `mirror unmount` for configured disks when its preflight accepts their state; otherwise manually resolve the observed state after inspection. An interrupted final JSONL record may be incomplete; use the preceding complete records and fresh inspection. Logs are not automatically deleted.

`mirror ensure-copies` explicitly ingests missing checksum metadata and fills missing copies. It verifies that existing data agrees before publication and refuses disagreements for repair. Every missing regular-file copy requires both its data path and adjacent sidecar path to be absent during preflight, before any directories are unlocked or files published. An existing sidecar blocks the entire plan even when its checksum matches; resolve such obstructions manually without discarding checksum evidence. Preview does not change mirror files or modes; publication rechecks the destination pair, uses verified temporary files, and never replaces an unexpected destination.

`mirror repair-copies` scans without changing data, modes, or checksum files, then persists a repair plan. A file's adjacent `.b2sum` file is its checksum **sidecar**. Each configured mirror supports each distinct checksum supplied by its current data or valid sidecar at most once: agreement does not double its vote. A surviving valid sidecar still votes when its data file is missing. Such sidecar-only entries are evidence, not repair targets: repair leaves their missing data and exact sidecar bytes unchanged. Exactly one candidate must be supported by more than half of **all configured mirrors**, including absent/unmounted mirrors in the denominator. Multiple qualifying candidates are refused, even if their support counts differ. A data copy must actually match the winner before repair can proceed. Missing and malformed sidecars supply no checksum evidence; sidecars of existing data can be reconstructed from consensus, and malformed bytes are preserved. No checksum is invented during scanning.

Use `mirror repair-copies --resume` to load the last pending plan without repeating the full scan. Repair does not reconcile symlink targets or fill missing data copies; use ensure-copies for missing copies after resolving disagreements.

`mirror lock` makes mirror directories read-only; `mirror unlock` makes them writable for deliberate changes.

## Configuration

`~/.mirror/disks` contains one GPT disk identifier (the whole-disk `PTUUID`) per line. Blank lines and lines beginning with `#` are ignored. `mirror mount` resolves these stable identifiers to current `/dev/sdX` names and rejects ambiguous or unexpected layouts.

The most recent repair run is stored atomically in `~/.mirror/repair-last.json`. A completed scan with unresolved repairs remains `pending`, including when the prompt is declined or an apply is interrupted. Resume keeps mount paths as its identity keys, rechecks the configured mirror count and mounted roots, and hashes only the listed files and metadata. Completion requires every repair target's data and sidecar to contain the desired checksum, not merely its original scanned state. Sidecar-only evidence records an empty actual checksum; both resume and completion require those data paths to remain absent and their sidecars to retain the exact scanned bytes. Log format v3 is a hard cutover: older logs may omit decisive sidecar-only evidence and require a fresh scan. Starting a new scan replaces the previous log.

All commands that operate on disks, including previews, acquire the same non-waiting exclusive lock at `~/.mirror/operation.lock`. A second command fails immediately. Do not remove or replace this file; the kernel releases the lock on process exit. Help requires no lock. Preview may write this runtime lock and a repair scan log, but does not change mirror files or permissions.

SIGINT/SIGTERM cancel inventory, prompts, and work between bounded reads/writes. Cleanup uses an independent timeout and releases the operation lock. Copy/repair directory guards retain descriptors for the original mirror roots until relocking finishes. If a root path disappears or is replaced, cleanup attempts to relock only the original filesystem, never the replacement path, and reports the root change. Kernel-blocked disk I/O may still delay exit. The lock coordinates mirror commands only: keep other programs and manual writers off these disks during operations. Directory modes are an accidental-write guard, not protection against their owner.

## Notes

Mirror replicates regular files and symlinks, creating parent directories as needed. Empty directories and directory-only trees are intentionally not replicated: they produce no copy actions, including in preview. Existing empty directories are not deleted. Directory structure is a container for mirrored entries, not independently mirrored data; other filesystem object types are unsupported.

Symlink targets are preserved exactly, without normalization or traversal. Relative targets such as `target`, `./target`, and `../target`, absolute targets, dangling links, and loops are all copied as links, without checksum sidecars. Ensure rejects differing target strings across copies and rechecks the source target before publication. Copying a link does not mirror the data it points to; a link can resolve outside the mirror when another program follows it.

The `.b2sum` suffix is reserved for checksum sidecars: regular payload files and symlinks must not end in `.b2sum`. Rename such application files before placing them in the mirror. A regular `name.b2sum` is always metadata for the regular payload `name`, never a payload itself; a sidecar cannot have its own sidecar. Directory names may end in `.b2sum`. Ensure refuses checksum-only paths with no regular data source on any mounted mirror, instead of silently reporting success. Repair still retains orphan sidecars as evidence, including malformed bytes without a vote. The suffix defines the namespace, not a guess about file contents: damaged metadata must not be automatically renamed or ingested as payload.

Only immutable data should go in the mirror. Make copies instead of mutations. Mutations will trigger repair.

Each disk reserves a private, marked `.mirror-internal/` directory. Verified replacements are staged in `tmp/`; corrupt data and sidecars are retained on the affected disk under `quarantine/<unique-record>/<original-relative-path>`. Neither subtree is scanned or replicated, and evidence is not automatically deleted. Existing unmarked content at this reserved path is rejected rather than adopted. Crash-left temporary files remain distinguishable from user data. Inspect them manually; do not move them into normal data without deciding what they represent. Older `.corrupted.N` files are not automatically migrated or inferred to be evidence; move any such known backups out of the mirrored object namespace before running ensure-copies.

Linux only; descriptor-rooted access requires kernel `openat2` support (Linux 5.6+). Repair and copy operations reject symlink traversal and nested mounts rather than falling back to path-based access.

## Testing

The normal suite exercises production Go state machines through injected operating-system boundaries. It never opens a block device:

```bash
go run ./bin/check.go
```

The check automation is a standard-library Go program. It runs formatting checks, unit tests, race tests, vet, staticcheck, module checks, and thin-wrapper syntax checks.

The opt-in end-to-end test uses the `vm` project and its `mirror-e2e` image. It boots an ephemeral VM with a blank, instance-only 1 GB secondary disk and launches the production `mirror format`, `mirror mount`, and `mirror unmount` commands through guest sudo using the real `fdisk`, `cryptsetup`, `mkfs.ext4`, `mount`, and `umount`. A guest-side checker verifies GPT, LUKS2, ext4 with zero reserved blocks, and caller UID/GID ownership. The unprivileged caller writes and syncs a payload once, then reads that same payload without recreating or truncating it after unmount/remount. The test finishes closed and unmounted. The VM receives no host block device, and `vm bake` never includes secondary disks in images.

Build the usage-specific image from a fresh `sid` base after changing its recipe:

```bash
bin/bake-e2e-image
```

Run the end-to-end test:

```bash
MIRROR_E2E_VM=1 go test -count=1 -run TestMirrorFormatVM -v ./tests/e2e
```

Set `MIRROR_E2E_KEEP=1` with `MIRROR_E2E_VM=1` to retain the test VM for debugging.
