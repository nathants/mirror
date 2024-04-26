# mirror

## why

s3 is the safest place to store immutable data, but it's expensive to fetch that data due to egress bandwidth fees of $0.10/GB.

we need something local to mirror that s3 data, that is equally safe, and free to access.

zfs is too complex. raid doesn't protect against bitrot. we need something simpler. something unborkable.

## how

several spinning rust disks form a local mirror.

write immutable data to any of them, then mirror it to all of them.

periodically run repair, which detects bitrot and repairs the data from any other disk that has an uncorrupted copy.

use a minimum of 2 disks. 3 is better. 4+ is fine for extreme durability.

## what

[mirror-format.sh](./bin/mirror-format.sh) - interactively select a disk to reformat with ext4 and encrypt with cryptsetup.

[mirror-mount](./bin/mirror-mount) - unlock and mount all drives listed in ~/.mirror by disk identifier.

[mirror-ensure-copies](./bin/mirror-ensure-copies) - scan each drive, then checksum and copy files as needed.

[mirror-repair-copies](./bin/mirror-repair-copies) - scan each drive, look for checksum mismatches, repair them from valid data if possible.

[mirror-lock](./bin/mirror-lock) - makes all files and directories read only, only needed if you unlocked.

[mirror-unlock](./bin/mirror-unlock) - unlock all files and directories in case you need to make deletions.

## notes

only immutable data should go in the mirror. make copies instead of mutations. mutations will trigger repair.

when repairing, corrupted files are renamed and not overwritten. they can be inspected later.
