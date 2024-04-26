# mirror

## why

s3 is the safest place to store immutable, but it's expensive to fetch that data due to egress bandwidth fees of $0.10/GB.

we need something local to mirror that s3 data, that is equally safe, and free to access.

zfs and raid are too complex, annoying, and we don't need speed beyond what a single drive can provide. we need something simpler. unborkable.

## how

several spinning rust disks form a local mirror.

write immutable data to any of them, then mirror it to all of them.

periodically run repair, which detects bitrot and repairs the data from any other disk that has an uncorrupted copy.

use a minimum of 2 disks. 3 is better. 4+ is fine for extreme durability use cases.

## what

[mirror-format.sh](./bin/mirror-format.sh) - interactively select a disk to reformat with ext4 and encrypt with cryptsetup.

[mirror-mount](./bin/mirror-mount) - unlock and mount all drives listed in ~/.mirror by disk identifier.

[mirror-ensure-copies](./bin/mirror-ensure-copies) - scan each drive, then checksum and copy files as needed.

[mirror-repair-copies](./bin/mirror-repair-copies) - scan each drive, look for checksum mismatches, repair them from valid data if possible.

[mirror-lock](./bin/mirror-lock) - makes all files and directories read only, this happens automatically when using `ensure` or `repair`.

[mirror-unlock](./bin/mirror-unlock) - unlock all files and directories in case you need to make deletions. don't forget to `lock` them again.

## notes

only immutable data should go in the mirror. make copies instead of mutations.

when repairing, corrupted files are renamed and not overwritten. they can later be inspected if needed.
