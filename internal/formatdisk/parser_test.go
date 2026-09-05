package formatdisk

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

func TestFormatInventoryRejectsTrailingBlockJSON(t *testing.T) {
	fake := newFakeSystem()
	data := string(marshalLSBLK(fake.rootDevice()))
	for _, suffix := range []string{" trailing", ` {"blockdevices":[]}`} {
		app := &app{system: parserResponseSystem{fakeSystem: fake, name: "lsblk", data: data + suffix}}
		if _, err := app.lsblk(context.Background(), true); err == nil {
			t.Errorf("accepted trailing lsblk content %q", suffix)
		}
	}
}

func TestFormatInventoryRejectsMalformedMountDocuments(t *testing.T) {
	for _, data := range []string{`{}`, `{"filesystems":null}`, `{"filesystems":[]}`, `{"filesystems":[{"target":""}]}`, `{"filesystems":[{"target":"/home/test/../other"}]}`} {
		app := &app{system: parserResponseSystem{fakeSystem: newFakeSystem(), name: "findmnt", data: data}}
		if err := app.assertMountpointSafe(context.Background(), "/home/test/mnt.sdz"); err == nil {
			t.Errorf("accepted invalid mount inventory %s", data)
		}
	}
}

func TestFormatUsesSharedCapturedFixtures(t *testing.T) {
	for _, shape := range []string{"flat", "tree"} {
		data, err := os.ReadFile("../inventory/testdata/lsblk-" + shape + ".json")
		if err != nil {
			t.Fatal(err)
		}
		app := &app{system: parserResponseSystem{fakeSystem: newFakeSystem(), name: "lsblk", data: string(data)}}
		disks, err := app.listDisks(context.Background())
		if err != nil || len(disks) != 1 || disks[0].Path != "/dev/sde" || disks[0].Serial != "FIXTURE-SERIAL" {
			t.Fatalf("format did not parse common %s fixture: %+v %v", shape, disks, err)
		}
		if _, err := app.inspectSafeDevice(context.Background(), "/dev/sde"); err == nil || !strings.Contains(err.Error(), "active holder") {
			t.Fatalf("shared parser bypassed format's holder policy: %v", err)
		}
		data, err = os.ReadFile("../inventory/testdata/findmnt-" + shape + ".json")
		if err != nil {
			t.Fatal(err)
		}
		app.system = parserResponseSystem{fakeSystem: newFakeSystem(), name: "findmnt", data: string(data)}
		if err := app.assertMountpointSafe(context.Background(), "/dev"); err == nil || !strings.Contains(err.Error(), "already mounted") {
			t.Fatalf("shared mount parser bypassed format's occupancy policy: %v", err)
		}
	}
}
