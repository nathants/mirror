package mountdisks

import (
	"context"
	"os"
	"strings"
	"testing"
)

type parserResponseSystem struct {
	*fakeSystem
	name, data string
}

func (s parserResponseSystem) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == s.name {
		return []byte(s.data), nil
	}
	return s.fakeSystem.Output(ctx, name, args...)
}

func TestMountInventoryRejectsMalformedDocuments(t *testing.T) {
	for _, data := range []string{`{}`, `{"filesystems":null}`, `{"filesystems":[]}`, `{"filesystems":[{"source":"/dev/x","target":""}]}`, `{"filesystems":[{"source":"/dev/x","target":"/home/test/../other"}]}`} {
		system := parserResponseSystem{fakeSystem: newFakeSystem(), name: "findmnt", data: data}
		if mounts, err := listMounts(context.Background(), system); err == nil {
			t.Errorf("accepted invalid mount inventory %s: %+v", data, mounts)
		}
	}
	for _, suffix := range []string{" trailing", ` {"blockdevices":[]}`} {
		system := newFakeSystem()
		data, err := system.Output(context.Background(), "lsblk")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := listBlockDevices(context.Background(), parserResponseSystem{fakeSystem: system, name: "lsblk", data: string(data) + suffix}); err == nil {
			t.Errorf("accepted trailing lsblk content %q", suffix)
		}
	}
}

func TestMountInventoryRetainsNestedAndStackedMounts(t *testing.T) {
	data := `{"filesystems":[{"source":"rootfs","target":"/","children":[{"source":"/dev/mapper/sde","target":"/home/test/mnt.sde"},{"source":"tmpfs","target":"/home/test/mnt.sde/nested"},{"source":"/dev/other","target":"/home/test/mnt.sde"}]}]}`
	mounts, err := listMounts(context.Background(), parserResponseSystem{fakeSystem: newFakeSystem(), name: "findmnt", data: data})
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts["/home/test/mnt.sde"]) != 2 || len(mounts["/home/test/mnt.sde/nested"]) != 1 {
		t.Fatalf("parser lost mount conflicts: %+v", mounts)
	}
	fake := newFakeSystem()
	_, _, err = inspectPlans(context.Background(), parserResponseSystem{fakeSystem: fake, name: "findmnt", data: data}, []string{fake.identifier}, "/home/test", "/dev/mapper")
	if err == nil || !strings.Contains(err.Error(), "nested mount") {
		t.Fatalf("nested inventory bypassed mount safety: %v", err)
	}
}

func TestMountUsesSharedCapturedFixtures(t *testing.T) {
	for _, shape := range []string{"flat", "tree"} {
		data, err := os.ReadFile("../inventory/testdata/lsblk-" + shape + ".json")
		if err != nil {
			t.Fatal(err)
		}
		fake := newFakeSystem()
		fake.identifier = "11111111-1111-4111-8111-111111111111"
		system := parserResponseSystem{fakeSystem: fake, name: "lsblk", data: string(data)}
		plans, missing, err := inspectPlans(context.Background(), system, []string{fake.identifier}, "/home/test", "/dev/mapper")
		if err != nil || len(plans) != 1 || len(missing) != 0 || plans[0].Mapper == nil || plans[0].Mapper.Path != "/dev/mapper/sde" || plans[0].Partition.ParentName != "/dev/sde" {
			t.Fatalf("mount did not parse common %s fixture: %+v %v", shape, plans, err)
		}
		if err := requireMountable(plans); err != nil {
			t.Fatal(err)
		}
		data, err = os.ReadFile("../inventory/testdata/findmnt-" + shape + ".json")
		if err != nil {
			t.Fatal(err)
		}
		mounts, err := listMounts(context.Background(), parserResponseSystem{fakeSystem: fake, name: "findmnt", data: string(data)})
		if err != nil || len(mounts) != 5 || len(mounts["/dev/pts"]) != 1 || mounts["/dev/pts"][0] != "devpts" {
			t.Fatalf("mount dropped common %s fixture evidence: %+v %v", shape, mounts, err)
		}
	}
}
