package mountdisks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const lsblkColumns = "PATH,KNAME,PKNAME,MAJ:MIN,SIZE,TYPE,FSTYPE,PTUUID,UUID,PARTUUID,MOUNTPOINTS,MODEL,SERIAL,WWN,RO"

var (
	wholeDiskPattern = regexp.MustCompile(`^/dev/sd[a-z]+$`)
	gptUUIDPattern   = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

type blockDevice struct {
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
	Children    []blockDevice `json:"children"`
}

type lsblkDocument struct {
	BlockDevices []blockDevice `json:"blockdevices"`
}

type mountRecord struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

type findmntDocument struct {
	Filesystems []mountRecord `json:"filesystems"`
}

type diskPlan struct {
	ConfigID      string
	Disk          blockDevice
	Partition     blockDevice
	Mapper        *blockDevice
	CryptName     string
	MapperPath    string
	Mountpoint    string
	MountedSource string
}

func loadConfig(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read mirror config %s: %w", path, err)
	}
	seen := make(map[string]struct{})
	var identifiers []string
	for lineNumber, line := range strings.Split(string(data), "\n") {
		identifier := strings.TrimSpace(line)
		if identifier == "" || strings.HasPrefix(identifier, "#") {
			continue
		}
		if !gptUUIDPattern.MatchString(identifier) {
			return nil, fmt.Errorf("invalid GPT disk identifier on line %d of %s: %q", lineNumber+1, path, identifier)
		}
		identifier = strings.ToLower(identifier)
		if _, exists := seen[identifier]; exists {
			return nil, fmt.Errorf("duplicate GPT disk identifier in %s: %s", path, identifier)
		}
		seen[identifier] = struct{}{}
		identifiers = append(identifiers, identifier)
	}
	if len(identifiers) == 0 {
		return nil, fmt.Errorf("mirror config contains no disk identifiers: %s", path)
	}
	return identifiers, nil
}

type Snapshot struct {
	Roots             []string
	Missing           []string
	ConfiguredMirrors int
}

func MountedRoots(ctx context.Context, system System, configPath, homeDir, mapperDir string) (Snapshot, error) {
	if system == nil {
		return Snapshot{}, errors.New("system is required")
	}
	if configPath == "" || !filepath.IsAbs(configPath) || filepath.Clean(configPath) != configPath {
		return Snapshot{}, errors.New("config path must be a clean absolute path")
	}
	if homeDir == "" || !filepath.IsAbs(homeDir) || filepath.Clean(homeDir) != homeDir {
		return Snapshot{}, errors.New("home directory must be a clean absolute path")
	}
	if mapperDir == "" {
		mapperDir = "/dev/mapper"
	}
	if !filepath.IsAbs(mapperDir) || filepath.Clean(mapperDir) != mapperDir {
		return Snapshot{}, errors.New("mapper directory must be a clean absolute path")
	}
	identifiers, err := loadConfig(configPath)
	if err != nil {
		return Snapshot{}, err
	}
	plans, missing, err := inspectPlans(ctx, system, identifiers, homeDir, mapperDir)
	if err != nil {
		return Snapshot{}, err
	}
	var roots []string
	for _, plan := range plans {
		if plan.MountedSource != "" {
			roots = append(roots, plan.Mountpoint)
		}
	}
	sort.Strings(roots)
	return Snapshot{Roots: roots, Missing: missing, ConfiguredMirrors: len(identifiers)}, nil
}

func inspectPlans(ctx context.Context, system System, identifiers []string, homeDir, mapperDir string) ([]diskPlan, []string, error) {
	devices, err := listBlockDevices(ctx, system)
	if err != nil {
		return nil, nil, err
	}
	mounts, err := listMounts(ctx, system)
	if err != nil {
		return nil, nil, err
	}

	byPath := make(map[string]blockDevice, len(devices))
	children := make(map[string][]blockDevice)
	for _, device := range devices {
		if device.Path == "" {
			return nil, nil, errors.New("lsblk returned a device without a path")
		}
		if _, exists := byPath[device.Path]; exists {
			return nil, nil, fmt.Errorf("lsblk returned a device path more than once: %s", device.Path)
		}
		byPath[device.Path] = device
		if device.ParentName != "" {
			children[device.ParentName] = append(children[device.ParentName], device)
		}
	}

	var plans []diskPlan
	var missing []string
	for _, identifier := range identifiers {
		var matches []blockDevice
		for _, device := range devices {
			if device.Type == "disk" && strings.EqualFold(device.PTUUID, identifier) {
				matches = append(matches, device)
			}
		}
		if len(matches) == 0 {
			missing = append(missing, identifier)
			continue
		}
		if len(matches) != 1 {
			return nil, nil, fmt.Errorf("GPT disk identifier %s resolved to %d devices", identifier, len(matches))
		}
		disk := matches[0]
		if !wholeDiskPattern.MatchString(disk.Path) {
			return nil, nil, fmt.Errorf("configured disk %s is not a whole /dev/sdX device: %s", identifier, disk.Path)
		}
		if disk.ReadOnly {
			return nil, nil, fmt.Errorf("configured disk is read-only: %s", disk.Path)
		}
		if disk.Serial == "" && disk.WWN == "" {
			return nil, nil, fmt.Errorf("configured disk has neither a serial nor a WWN: %s", disk.Path)
		}
		if disk.Filesystem != "" || len(deviceMountpoints(disk)) != 0 {
			return nil, nil, fmt.Errorf("configured whole disk has an unexpected filesystem or mount: %s", disk.Path)
		}

		partitions := children[disk.Path]
		if len(partitions) != 1 || partitions[0].Type != "part" || partitions[0].Path != disk.Path+"1" {
			return nil, nil, fmt.Errorf("configured disk must have exactly one partition at %s1: %s", disk.Path, disk.Path)
		}
		partition := partitions[0]
		if partition.ReadOnly {
			return nil, nil, fmt.Errorf("configured partition is read-only: %s", partition.Path)
		}
		if !strings.EqualFold(partition.Filesystem, "crypto_LUKS") {
			return nil, nil, fmt.Errorf("configured partition is not a LUKS container: %s has type %q", partition.Path, partition.Filesystem)
		}
		if len(deviceMountpoints(partition)) != 0 {
			return nil, nil, fmt.Errorf("configured LUKS partition is mounted directly: %s", partition.Path)
		}

		cryptName := filepath.Base(disk.Path)
		mapperPath := filepath.Join(mapperDir, cryptName)
		mountpoint := filepath.Join(homeDir, "mnt."+cryptName)
		for target := range mounts {
			if strings.HasPrefix(target, mountpoint+string(filepath.Separator)) {
				return nil, nil, fmt.Errorf("mountpoint contains a nested mount at %s: %s", target, mountpoint)
			}
		}
		partitionChildren := children[partition.Path]
		var mapper *blockDevice
		for index := range partitionChildren {
			candidate := partitionChildren[index]
			if candidate.Path != mapperPath || candidate.Type != "crypt" {
				return nil, nil, fmt.Errorf("configured partition has an unexpected active holder at %s: %s", candidate.Path, partition.Path)
			}
			if mapper != nil {
				return nil, nil, fmt.Errorf("mapper appears more than once: %s", mapperPath)
			}
			copy := candidate
			mapper = &copy
		}

		sources := mounts[mountpoint]
		if len(sources) > 1 {
			return nil, nil, fmt.Errorf("mountpoint has multiple stacked mounts: %s", mountpoint)
		}
		mountedSource := ""
		if len(sources) == 1 {
			mountedSource = sources[0]
			if mountedSource != mapperPath {
				return nil, nil, fmt.Errorf("mountpoint %s is occupied by %s, expected %s", mountpoint, mountedSource, mapperPath)
			}
		}

		if mapper == nil {
			if mountedSource != "" {
				return nil, nil, fmt.Errorf("mountpoint reports %s but mapper is absent: %s", mountedSource, mapperPath)
			}
		} else {
			if mapper.ReadOnly {
				return nil, nil, fmt.Errorf("mapper is read-only: %s", mapperPath)
			}
			if len(children[mapperPath]) != 0 {
				return nil, nil, fmt.Errorf("mapper has unexpected active holders: %s", mapperPath)
			}
			if !strings.EqualFold(mapper.Filesystem, "ext4") {
				return nil, nil, fmt.Errorf("mapper does not contain ext4: %s has type %q", mapperPath, mapper.Filesystem)
			}
			mapperMounts := deviceMountpoints(*mapper)
			sort.Strings(mapperMounts)
			if len(mapperMounts) > 1 || (len(mapperMounts) == 1 && mapperMounts[0] != mountpoint) {
				return nil, nil, fmt.Errorf("mapper is mounted somewhere unexpected: %s at %v", mapperPath, mapperMounts)
			}
			if (len(mapperMounts) == 1) != (mountedSource != "") {
				return nil, nil, fmt.Errorf("lsblk and findmnt disagree about mount state for %s", mapperPath)
			}
		}

		plans = append(plans, diskPlan{
			ConfigID:      identifier,
			Disk:          disk,
			Partition:     partition,
			Mapper:        mapper,
			CryptName:     cryptName,
			MapperPath:    mapperPath,
			Mountpoint:    mountpoint,
			MountedSource: mountedSource,
		})
	}
	return plans, missing, nil
}

func deviceMountpoints(device blockDevice) []string {
	var mountpoints []string
	for _, value := range device.Mountpoints {
		if value != nil && *value != "" {
			mountpoints = append(mountpoints, *value)
		}
	}
	return mountpoints
}

func listBlockDevices(ctx context.Context, system System) ([]blockDevice, error) {
	output, err := system.Output(ctx, "lsblk", "--json", "--bytes", "--paths", "--output", lsblkColumns)
	if err != nil {
		return nil, fmt.Errorf("list block devices: %w", err)
	}
	var document lsblkDocument
	decoder := json.NewDecoder(bytes.NewReader(output))
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode lsblk JSON: %w", err)
	}
	var flattened []blockDevice
	for _, device := range document.BlockDevices {
		flattened = appendFlattened(flattened, device)
	}
	return flattened, nil
}

func appendFlattened(destination []blockDevice, device blockDevice) []blockDevice {
	children := device.Children
	device.Children = nil
	destination = append(destination, device)
	for _, child := range children {
		destination = appendFlattened(destination, child)
	}
	return destination
}

func listMounts(ctx context.Context, system System) (map[string][]string, error) {
	output, err := system.Output(ctx, "findmnt", "--json", "--list", "--output", "SOURCE,TARGET")
	if err != nil {
		return nil, fmt.Errorf("list mounted filesystems: %w", err)
	}
	var document findmntDocument
	if err := json.Unmarshal(output, &document); err != nil {
		return nil, fmt.Errorf("decode findmnt JSON: %w", err)
	}
	mounts := make(map[string][]string)
	for _, record := range document.Filesystems {
		if record.Target == "" {
			continue
		}
		if !filepath.IsAbs(record.Target) {
			return nil, fmt.Errorf("findmnt returned a non-absolute target: %q", record.Target)
		}
		mounts[filepath.Clean(record.Target)] = append(mounts[filepath.Clean(record.Target)], record.Source)
	}
	return mounts, nil
}
