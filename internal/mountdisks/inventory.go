package mountdisks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
	"mirror/internal/cancelio"
	"mirror/internal/inventory"
)

var (
	wholeDiskPattern = regexp.MustCompile(`^/dev/sd[a-z]+$`)
	gptUUIDPattern   = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

type diskPlan struct {
	ConfigID      string
	Disk          inventory.BlockDevice
	Partition     inventory.BlockDevice
	Mapper        *inventory.BlockDevice
	CryptName     string
	MapperPath    string
	Mountpoint    string
	MountedSource string
}

// A disk-ID list is small metadata, including its comments and whitespace.
const maxConfigBytes = 64 * 1024

func loadConfig(ctx context.Context, path string) (identifiers []string, returnErr error) {
	defer func() {
		if returnErr != nil {
			identifiers = nil
			returnErr = fmt.Errorf("read mirror config %s: %w", path, returnErr)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// NO_SYMLINKS applies to the complete path, not just the final entry.
	// NONBLOCK prevents a FIFO open from waiting before we can inspect its type.
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_NONBLOCK | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { returnErr = errors.Join(returnErr, ctx.Err(), file.Close()) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("config must be a regular file")
	}
	if info.Size() > maxConfigBytes {
		return nil, fmt.Errorf("config exceeds %d-byte limit", maxConfigBytes)
	}
	return readConfig(ctx, path, file)
}

func readConfig(ctx context.Context, path string, reader io.Reader) ([]string, error) {
	// Do not trust stat size alone: the file can grow or report a synthetic size.
	data, err := io.ReadAll(io.LimitReader(cancelio.Reader{Context: ctx, Reader: reader}, maxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("config exceeds %d-byte limit", maxConfigBytes)
	}
	seen := make(map[string]struct{})
	var identifiers []string
	for lineNumber, line := range strings.Split(string(data), "\n") {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
	identifiers, err := loadConfig(ctx, configPath)
	if err != nil {
		return Snapshot{}, err
	}
	plans, missing, err := inspectPlans(ctx, system, identifiers, homeDir, mapperDir)
	if err != nil {
		return Snapshot{}, err
	}
	if err := requireMountable(plans); err != nil {
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

	seenPaths := make(map[string]struct{}, len(devices))
	children := make(map[string][]inventory.BlockDevice)
	for _, device := range devices {
		if _, exists := seenPaths[device.Path]; exists {
			return nil, nil, fmt.Errorf("lsblk returned a device path more than once: %s", device.Path)
		}
		seenPaths[device.Path] = struct{}{}
		if device.ParentName != "" {
			children[device.ParentName] = append(children[device.ParentName], device)
		}
	}

	var plans []diskPlan
	var missing []string
	for _, identifier := range identifiers {
		var matches []inventory.BlockDevice
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
		if disk.Serial == "" && disk.WWN == "" {
			return nil, nil, fmt.Errorf("configured disk has neither a serial nor a WWN: %s", disk.Path)
		}
		if disk.Filesystem != "" || len(disk.ActiveMountpoints()) != 0 {
			return nil, nil, fmt.Errorf("configured whole disk has an unexpected filesystem or mount: %s", disk.Path)
		}

		partitions := children[disk.Path]
		if len(partitions) != 1 || partitions[0].Type != "part" || partitions[0].Path != disk.Path+"1" {
			return nil, nil, fmt.Errorf("configured disk must have exactly one partition at %s1: %s", disk.Path, disk.Path)
		}
		partition := partitions[0]
		if !strings.EqualFold(partition.Filesystem, "crypto_LUKS") {
			return nil, nil, fmt.Errorf("configured partition is not a LUKS container: %s has type %q", partition.Path, partition.Filesystem)
		}
		if len(partition.ActiveMountpoints()) != 0 {
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
		var mapper *inventory.BlockDevice
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
			if len(children[mapperPath]) != 0 {
				return nil, nil, fmt.Errorf("mapper has unexpected active holders: %s", mapperPath)
			}
			mapperMounts := mapper.ActiveMountpoints()
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

// requireMountable is additional policy for mounting and mounted-root access,
// not for deactivation. Identity, holder, and mount checks stay in inspectPlans.
func requireMountable(plans []diskPlan) error {
	for _, plan := range plans {
		if plan.Disk.ReadOnly {
			return fmt.Errorf("configured disk is read-only: %s", plan.Disk.Path)
		}
		if plan.Partition.ReadOnly {
			return fmt.Errorf("configured partition is read-only: %s", plan.Partition.Path)
		}
		if plan.Mapper != nil {
			if plan.Mapper.ReadOnly {
				return fmt.Errorf("mapper is read-only: %s", plan.MapperPath)
			}
			if !strings.EqualFold(plan.Mapper.Filesystem, "ext4") {
				return fmt.Errorf("mapper does not contain ext4: %s has type %q", plan.MapperPath, plan.Mapper.Filesystem)
			}
		}
	}
	return nil
}

func listBlockDevices(ctx context.Context, system System) ([]inventory.BlockDevice, error) {
	output, err := system.Output(ctx, "lsblk", "--json", "--bytes", "--paths", "--output", inventory.BlockColumns)
	if err != nil {
		return nil, fmt.Errorf("list block devices: %w", err)
	}
	return inventory.ParseBlocks(output)
}

func listMounts(ctx context.Context, system System) (map[string][]string, error) {
	output, err := system.Output(ctx, "findmnt", "--json", "--list", "--output", "SOURCE,TARGET")
	if err != nil {
		return nil, fmt.Errorf("list mounted filesystems: %w", err)
	}
	records, err := inventory.ParseMounts(output)
	if err != nil {
		return nil, err
	}
	mounts := make(map[string][]string)
	for _, record := range records {
		if record.Source == "" {
			return nil, fmt.Errorf("findmnt returned no source for %s", record.Target)
		}
		mounts[record.Target] = append(mounts[record.Target], record.Source)
	}
	return mounts, nil
}
