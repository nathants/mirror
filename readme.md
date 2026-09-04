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

`mirror ensure-copies` scans each mounted mirror disk, then checksums and copies files as needed.

`mirror repair-copies` scans each mounted mirror disk, persists the repair plan, and repairs mismatches only when the data and sidecar checksums establish a strict majority with a matching source copy. Use `--resume` to load the last pending plan without repeating the full scan.

`mirror lock` makes mirror directories read-only; `mirror unlock` makes them writable for deliberate changes.

## Configuration

`~/.mirror/disks` contains one GPT disk identifier (the whole-disk `PTUUID`) per line. Blank lines and lines beginning with `#` are ignored. `mirror mount` resolves these stable identifiers to current `/dev/sdX` names and rejects ambiguous or unexpected layouts.

The most recent repair run is stored atomically in `~/.mirror/repair-last.json`. A completed scan with unresolved repairs remains `pending`, including when the prompt is declined or the process is interrupted. `mirror repair-copies --resume` displays that plan immediately, asks for confirmation, then rechecks the configured mounts and hashes only the listed files before changing anything. A completed repair marks the log `completed`; a clean scan marks it `clean`.

## Notes

Only files, directories, and symlinks are supported.

Only immutable data should go in the mirror. Make copies instead of mutations. Mutations will trigger repair.

When repairing, corrupt bytes are retained under a `.corrupted.N` name before an atomically published replacement takes the original path. They can be inspected later.

Linux only.

## Testing

The normal suite exercises production Go state machines through injected operating-system boundaries. It never opens a block device:

```bash
go run ./bin/check.go
```

The check automation is a standard-library Go program. It runs formatting checks, unit tests, race tests, vet, staticcheck, module checks, and thin-wrapper syntax checks.

The opt-in end-to-end test uses the `vm` project and its `mirror-e2e` image. It boots an ephemeral VM with a blank, instance-only 1 GB secondary disk and launches the production `mirror format`, `mirror mount`, and `mirror unmount` commands through guest sudo using the real `fdisk`, `cryptsetup`, `mkfs.ext4`, `mount`, and `umount`. A guest-side checker verifies GPT, LUKS2, ext4 with zero reserved blocks, a data round trip, and final closed/unmounted state. The VM receives no host block device, and `vm bake` never includes secondary disks in images.

Build the usage-specific image from a fresh `sid` base after changing its recipe:

```bash
bin/bake-e2e-image
```

Run the end-to-end test:

```bash
MIRROR_E2E_VM=1 go test -count=1 -run TestMirrorFormatVM -v ./tests/e2e
```

Set `MIRROR_E2E_KEEP=1` with `MIRROR_E2E_VM=1` to retain the test VM for debugging.
