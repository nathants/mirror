package inventory

import (
	"os"
	"reflect"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCapturedBlockFixtures(t *testing.T) {
	flat, err := ParseBlocks(fixture(t, "lsblk-flat"))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := ParseBlocks(fixture(t, "lsblk-tree"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(flat, tree) || len(flat) != 3 {
		t.Fatalf("flat/tree mismatch: %+v / %+v", flat, tree)
	}
	mapper, disk, partition := flat[0], flat[1], flat[2]
	if disk.Path != "/dev/sde" || disk.Size != 4000787030016 || disk.PTUUID != "11111111-1111-4111-8111-111111111111" || disk.Serial != "FIXTURE-SERIAL" || disk.ReadOnly {
		t.Fatalf("disk evidence changed: %+v", disk)
	}
	if partition.ParentName != disk.Path || partition.Filesystem != "crypto_LUKS" || partition.UUID != "22222222-2222-4222-8222-222222222222" || partition.PartUUID != "33333333-3333-4333-8333-333333333333" {
		t.Fatalf("partition evidence changed: %+v", partition)
	}
	if mapper.ParentName != partition.Path || mapper.Path != "/dev/mapper/sde" || mapper.KernelName != "/dev/dm-0" || mapper.Filesystem != "ext4" || mapper.PTUUID != "" || len(mapper.ActiveMountpoints()) != 0 {
		t.Fatalf("mapper/null evidence changed: %+v", mapper)
	}
	for _, device := range flat {
		if device.Children != nil {
			t.Fatal("flattened inventory retained nested copies")
		}
	}
}

func TestCapturedMountFixtures(t *testing.T) {
	flat, err := ParseMounts(fixture(t, "findmnt-flat"))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := ParseMounts(fixture(t, "findmnt-tree"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Mount{{Source: "none", Target: "/dev"}, {Source: "hugetlbfs", Target: "/dev/hugepages"}, {Source: "mqueue", Target: "/dev/mqueue"}, {Source: "devpts", Target: "/dev/pts"}, {Source: "none", Target: "/dev/shm"}}
	if !reflect.DeepEqual(flat, want) || !reflect.DeepEqual(tree, want) {
		t.Fatalf("mount evidence changed: %+v / %+v", flat, tree)
	}
}

func TestParsersRejectIncompleteOrMalformedDocuments(t *testing.T) {
	for _, data := range []string{`{}`, `null`, `{"blockdevices":null}`, `{"blockdevices":[]} trailing`, `{"blockdevices":[]} {}`, `{"blockdevices":[{"path":""}]}`, `{"blockdevices":[{"path":"relative"}]}`, `{"blockdevices":[{"path":"/dev/../sde"}]}`, `{"blockdevices":[{"path":"/dev/sde","size":-1}]}`, `{"blockdevices":[{"path":"/dev/sde","ro":0}]}`, `{"blockdevices":[{"path":"/dev/sde","children":[null]}]}`} {
		if result, err := ParseBlocks([]byte(data)); err == nil || result != nil {
			t.Errorf("invalid blocks accepted: %s -> %+v %v", data, result, err)
		}
	}
	for _, data := range []string{`{}`, `null`, `{"filesystems":null}`, `{"filesystems":[]}`, `{"filesystems":[{"target":"/"}]} trailing`, `{"filesystems":[{"target":"/"}]} {}`, `{"filesystems":[{"target":""}]}`, `{"filesystems":[{"target":"relative"}]}`, `{"filesystems":[{"target":"/home/test/../other"}]}`, `{"filesystems":[{"target":"/","children":[{"target":""}]}]}`} {
		if result, err := ParseMounts([]byte(data)); err == nil || result != nil {
			t.Errorf("invalid mounts accepted: %s -> %+v %v", data, result, err)
		}
	}
	if devices, err := ParseBlocks([]byte(`{"blockdevices":[]}`)); err != nil || len(devices) != 0 {
		t.Fatalf("empty disk inventory is valid: %+v %v", devices, err)
	}
}

func TestParsersPreserveConflictAndOpaqueEvidence(t *testing.T) {
	devices, err := ParseBlocks([]byte(`{"blockdevices":[{"path":"/dev/sdz","mountpoints":["/z",null,"[SWAP]","/a","/a",""],"ro":true},{"path":"/dev/sdz"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 || !devices[0].ReadOnly || !reflect.DeepEqual(devices[0].ActiveMountpoints(), []string{"/a", "/a", "/z", "[SWAP]"}) {
		t.Fatalf("lost device/mount evidence: %+v", devices)
	}
	mounts, err := ParseMounts([]byte(`{"filesystems":[{"target":"/","children":[{"target":"/data","source":"/dev/disk[/subvolume]"},{"target":"/data","source":"tmpfs"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 3 || mounts[0].Source != "" || mounts[1].Source != "/dev/disk[/subvolume]" || mounts[2].Target != "/data" {
		t.Fatalf("lost target-only/stacked/opaque source evidence: %+v", mounts)
	}
}
