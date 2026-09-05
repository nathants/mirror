// Package inventory decodes the read-only util-linux JSON boundary shared by
// formatting and mounting. It validates representation, not storage eligibility.
package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
)

const BlockColumns = "PATH,KNAME,PKNAME,MAJ:MIN,SIZE,TYPE,FSTYPE,PTUUID,UUID,PARTUUID,MOUNTPOINTS,MODEL,SERIAL,WWN,RO"

type BlockDevice struct {
	Path        string        `json:"path"`
	KernelName  string        `json:"kname"`
	ParentName  string        `json:"pkname"`
	MajorMinor  string        `json:"maj:min"`
	Size        uint64        `json:"size"`
	Type        string        `json:"type"`
	Filesystem  string        `json:"fstype"`
	PTUUID      string        `json:"ptuuid"`
	UUID        string        `json:"uuid"`
	PartUUID    string        `json:"partuuid"`
	Mountpoints []*string     `json:"mountpoints"`
	Model       string        `json:"model"`
	Serial      string        `json:"serial"`
	WWN         string        `json:"wwn"`
	ReadOnly    bool          `json:"ro"`
	Children    []BlockDevice `json:"children,omitempty"`
}

type BlockDocument struct {
	BlockDevices []BlockDevice `json:"blockdevices"`
}

type Mount struct {
	Source   string  `json:"source"`
	Target   string  `json:"target"`
	Children []Mount `json:"children,omitempty"`
}

type MountDocument struct {
	Filesystems []Mount `json:"filesystems"`
}

// ParseBlocks accepts flat or tree output, returning a flat path-sorted snapshot
// with sorted mountpoint evidence for stable comparisons. Do not deduplicate:
// repeated devices/holders must remain visible to the caller's topology checks.
func ParseBlocks(data []byte) ([]BlockDevice, error) {
	var document BlockDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("decode lsblk JSON: %w", err)
	}
	if document.BlockDevices == nil {
		return nil, errors.New("lsblk JSON requires a blockdevices array")
	}
	var devices []BlockDevice
	var flatten func([]BlockDevice) error
	flatten = func(nodes []BlockDevice) error {
		for _, device := range nodes {
			if !cleanAbsolute(device.Path) {
				return fmt.Errorf("lsblk returned an invalid device path %q", device.Path)
			}
			children := device.Children
			device.Children = nil
			sort.SliceStable(device.Mountpoints, func(i, j int) bool {
				return mountpointValue(device.Mountpoints[i]) < mountpointValue(device.Mountpoints[j])
			})
			devices = append(devices, device)
			if err := flatten(children); err != nil {
				return err
			}
		}
		return nil
	}
	if err := flatten(document.BlockDevices); err != nil {
		return nil, err
	}
	sort.SliceStable(devices, func(i, j int) bool { return devices[i].Path < devices[j].Path })
	return devices, nil
}

// ParseMounts accepts a full findmnt inventory, flat or tree. An empty inventory
// is not evidence of an unoccupied mountpoint: even an otherwise empty system
// has a root mount. Sources remain opaque (e.g. tmpfs or /dev/disk[/subvolume]);
// target-only queries may omit them. Callers requiring sources check them.
// Stacked targets are deliberately retained, not merged or deduplicated.
func ParseMounts(data []byte) ([]Mount, error) {
	var document MountDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("decode findmnt JSON: %w", err)
	}
	if len(document.Filesystems) == 0 {
		return nil, errors.New("findmnt returned an empty mount inventory")
	}
	var mounts []Mount
	var flatten func([]Mount) error
	flatten = func(nodes []Mount) error {
		for _, mount := range nodes {
			if !cleanAbsolute(mount.Target) {
				return fmt.Errorf("findmnt returned an invalid target path %q", mount.Target)
			}
			children := mount.Children
			mount.Children = nil
			mounts = append(mounts, mount)
			if err := flatten(children); err != nil {
				return err
			}
		}
		return nil
	}
	if err := flatten(document.Filesystems); err != nil {
		return nil, err
	}
	sort.SliceStable(mounts, func(i, j int) bool {
		if mounts[i].Target != mounts[j].Target {
			return mounts[i].Target < mounts[j].Target
		}
		return mounts[i].Source < mounts[j].Source
	})
	return mounts, nil
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

func mountpointValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// ActiveMountpoints omits util-linux's null/empty placeholders but preserves
// all real evidence, including duplicates and the non-path [SWAP] marker.
func (d BlockDevice) ActiveMountpoints() []string {
	var paths []string
	for _, value := range d.Mountpoints {
		if path := mountpointValue(value); path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}
