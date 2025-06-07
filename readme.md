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

## What

[mirror_format.sh](https://github.com/nathants/mirror/tree/master/bin/mirror_format.sh) - interactively select a disk to reformat with ext4 and encrypt with cryptsetup.

[mirror-mount](https://github.com/nathants/mirror/tree/master/bin/mirror-mount) - unlock and mount all drives listed in ~/.mirror by disk identifier.

[mirror-ensure-copies](https://github.com/nathants/mirror/tree/master/cmd/mirror_ensure_copies.go) - scan each drive, then checksum and copy files as needed.

[mirror-repair-copies](https://github.com/nathants/mirror/tree/master/cmd/mirror_repair_copies.go) - scan each drive, look for checksum mismatches, repair them from valid data if possible.

[mirror-lock](https://github.com/nathants/mirror/tree/master/cmd/mirror_lock.go) - makes all files and directories read only, only needed if you unlocked.

[mirror-unlock](https://github.com/nathants/mirror/tree/master/cmd/mirror_unlock.go) - unlock all files and directories in case you need to make deletions.

## Notes

Only files, directories, and symlinks are supported.

Only immutable data should go in the mirror. Make copies instead of mutations. Mutations will trigger repair.

When repairing, corrupted files are renamed and not overwritten. They can be inspected later.

Linux only. Might work on macOS or Windows with modifications.