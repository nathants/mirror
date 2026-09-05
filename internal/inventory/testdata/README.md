# Inventory fixtures

Captured with read-only util-linux 2.42.2 commands on 2026-09-05. Tests read these checked-in bytes; they never run host inventory or storage tools.

- `lsblk-flat.json`: `lsblk --json --bytes --paths --output PATH,KNAME,PKNAME,MAJ:MIN,SIZE,TYPE,FSTYPE,PTUUID,UUID,PARTUUID,MOUNTPOINTS,MODEL,SERIAL,WWN,RO -- /dev/sde`
- `lsblk-tree.json`: the same query with `--tree`.
- `findmnt-flat.json`: `findmnt --json --list --submounts --output SOURCE,TARGET --target /dev`
- `findmnt-tree.json`: the same query without `--list`.

The lsblk disk model, serial, WWN, and UUID values were replaced with fixture identities. Device paths, numeric sizes, booleans, nulls, empty mountpoint arrays, and parent/child relationships retain the captured representation. JSON was reindented. The block fixtures describe an unmounted LUKS/ext4 mapper; the mount fixtures are a bounded subtree of pseudo-filesystems, not a claim about the current host or a complete host inventory.

Additional malformed, stacked/nested, swap-marker, and null-mountpoint cases are literal JSON in tests, independent of Go struct marshaling.
