package mountdisks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

// Alter only injected inventory, never host devices or the production parser.
type recoverySystem struct {
	*fakeSystem
	inventories int
	alter       func(int, *blockDevice)
}

func (s *recoverySystem) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	data, err := s.fakeSystem.Output(ctx, name, args...)
	if err != nil || name != "lsblk" {
		return data, err
	}
	var document lsblkDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	s.inventories++
	s.alter(s.inventories, &document.BlockDevices[0])
	return json.Marshal(document)
}

func TestUnmountDoesNotRequireMountableFilesystem(t *testing.T) {
	for _, test := range []struct {
		name  string
		alter func(*blockDevice)
	}{
		{"unknown filesystem", func(d *blockDevice) { d.Children[0].Children[0].Filesystem = "" }},
		{"non-ext4 filesystem", func(d *blockDevice) { d.Children[0].Children[0].Filesystem = "xfs" }},
		{"read-only disk", func(d *blockDevice) { d.ReadOnly = true }},
		{"read-only partition", func(d *blockDevice) { d.Children[0].ReadOnly = true }},
		{"read-only mapper", func(d *blockDevice) { d.Children[0].Children[0].ReadOnly = true }},
	} {
		for _, mounted := range []bool{false, true} {
			name := test.name + "/open only"
			if mounted {
				name = test.name + "/mounted"
			}
			t.Run(name, func(t *testing.T) {
				base := newFakeSystem()
				base.mapperOpen, base.mounted, base.mountpointExists = true, mounted, mounted
				system := &recoverySystem{fakeSystem: base, alter: func(_ int, disk *blockDevice) { test.alter(disk) }}
				option := options(t, func() ([]byte, error) { t.Fatal("recovery requested password"); return nil, nil }, base.identifier)
				// Relaxing recovery must not admit unhealthy roots for mounting,
				// copying, repair, or directory guards.
				if _, err := MountedRoots(context.Background(), system, option.ConfigPath, option.HomeDir, option.MapperDir); err == nil {
					t.Fatal("mounted-root discovery accepted an unmount-only state")
				}
				if err := Mount(context.Background(), system, strings.NewReader("mount\n"), io.Discard, io.Discard, option); err == nil || strings.Contains(err.Error(), "nothing to mount") {
					t.Fatalf("mount did not reject unhealthy inventory: %v", err)
				}
				if len(mutatingCalls(base.calls)) != 0 {
					t.Fatal("mountability rejection mutated storage")
				}
				if err := Unmount(context.Background(), system, strings.NewReader("no\n"), io.Discard, io.Discard, option); !errors.Is(err, ErrDeclined) {
					t.Fatalf("recovery did not reach required confirmation: %v", err)
				}
				if len(mutatingCalls(base.calls)) != 0 {
					t.Fatal("declined recovery mutated storage")
				}
				if err := Unmount(context.Background(), system, strings.NewReader("unmount\n"), io.Discard, io.Discard, option); err != nil {
					t.Fatalf("identified mapper could not be deactivated: %v", err)
				}
				if base.mapperOpen || base.mounted {
					t.Fatal("recovery left the mapper or mount active")
				}
				want := []recordedCall{{name: "cryptsetup", rawName: "cryptsetup", args: []string{"close", "sde"}}}
				if mounted {
					want = append([]recordedCall{{name: "umount", rawName: "umount", args: []string{base.mountpoint}}}, want...)
				}
				if got := mutatingCalls(base.calls); !reflect.DeepEqual(got, want) {
					t.Fatalf("unexpected recovery commands: %+v", got)
				}
			})
		}
	}
}

func TestUnmountRecoveryRetainsIdentityAndTopologyChecks(t *testing.T) {
	for _, test := range []struct {
		name  string
		alter func(*blockDevice)
	}{
		{"missing hardware identity", func(d *blockDevice) { d.Serial, d.WWN = "", "" }},
		{"wrong partition type", func(d *blockDevice) { d.Children[0].Filesystem = "ext4" }},
		{"unexpected mapper", func(d *blockDevice) { d.Children[0].Children[0].Path = "/dev/mapper/other" }},
		{"unexpected holder", func(d *blockDevice) {
			m := &d.Children[0].Children[0]
			m.Children = []blockDevice{{Path: "/dev/dm-2", ParentName: m.Path, Type: "lvm"}}
		}},
		{"unexpected mount", func(d *blockDevice) {
			target := "/elsewhere"
			d.Children[0].Children[0].Mountpoints = []*string{&target}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := newFakeSystem()
			base.mapperOpen = true
			system := &recoverySystem{fakeSystem: base, alter: func(_ int, disk *blockDevice) {
				disk.ReadOnly = true
				disk.Children[0].Children[0].Filesystem = ""
				test.alter(disk)
			}}
			if err := Unmount(context.Background(), system, strings.NewReader("unmount\n"), io.Discard, io.Discard, options(t, nil, base.identifier)); err == nil {
				t.Fatal("recovery accepted unsafe identity/topology")
			}
			if len(mutatingCalls(base.calls)) != 0 {
				t.Fatal("unsafe recovery reached mutation")
			}
		})
	}
}

func TestUnmountRecoveryRevalidatesAfterConfirmation(t *testing.T) {
	base := newFakeSystem()
	base.mapperOpen = true
	system := &recoverySystem{fakeSystem: base, alter: func(n int, disk *blockDevice) {
		disk.Children[0].Children[0].Filesystem = ""
		if n == 2 {
			disk.Serial = "changed identity"
		}
	}}
	err := Unmount(context.Background(), system, strings.NewReader("unmount\n"), io.Discard, io.Discard, options(t, nil, base.identifier))
	if err == nil || !strings.Contains(err.Error(), "changed while awaiting confirmation") {
		t.Fatalf("unmount-only state was not revalidated: %v", err)
	}
	if len(mutatingCalls(base.calls)) != 0 {
		t.Fatal("identity drift reached mutation")
	}
}
