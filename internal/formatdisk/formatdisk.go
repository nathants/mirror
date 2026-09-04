package formatdisk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"mirror/internal/terminal"
)

const lsblkColumns = "PATH,KNAME,PKNAME,MAJ:MIN,SIZE,TYPE,FSTYPE,UUID,PARTUUID,MOUNTPOINTS,MODEL,SERIAL,WWN,RO"

var (
	productionDevicePattern = regexp.MustCompile(`^/dev/sd[a-z]+$`)
	testLoopDevicePattern   = regexp.MustCompile(`^/dev/loop[0-9]+$`)
)

type Options struct {
	HomeDir                     string
	MapperDir                   string
	OwnerUID                    int
	OwnerGID                    int
	AllowNonSCSIBlockForTesting bool
	PartitionPollAttempts       int
	PartitionPollInterval       time.Duration
}

type blockDevice struct {
	Path        string        `json:"path"`
	KernelName  string        `json:"kname"`
	ParentName  string        `json:"pkname"`
	MajorMinor  string        `json:"maj:min"`
	Size        uint64        `json:"size"`
	Type        string        `json:"type"`
	Filesystem  string        `json:"fstype"`
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
	Target   string        `json:"target"`
	Children []mountRecord `json:"children"`
}

type findmntDocument struct {
	Filesystems []mountRecord `json:"filesystems"`
}

type app struct {
	system       System
	input        io.Reader
	commandInput io.Reader
	out          io.Writer
	errOut       io.Writer
	option       Options
}

func Run(
	ctx context.Context,
	system System,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	option Options,
) (returnErr error) {
	if system == nil {
		return errors.New("system is required")
	}
	if stdin == nil || stdout == nil || stderr == nil {
		return errors.New("stdin, stdout, and stderr are required")
	}
	if option.HomeDir == "" || !filepath.IsAbs(option.HomeDir) || filepath.Clean(option.HomeDir) != option.HomeDir {
		return errors.New("home directory must be a clean absolute path")
	}
	if option.OwnerUID < 0 || option.OwnerGID < 0 {
		return errors.New("owner UID and GID must not be negative")
	}
	if option.MapperDir == "" {
		option.MapperDir = "/dev/mapper"
	}
	if !filepath.IsAbs(option.MapperDir) || filepath.Clean(option.MapperDir) != option.MapperDir {
		return errors.New("mapper directory must be a clean absolute path")
	}
	if option.PartitionPollAttempts == 0 {
		option.PartitionPollAttempts = 100
	}
	if option.PartitionPollAttempts < 1 {
		return errors.New("partition poll attempts must be positive")
	}
	if option.PartitionPollInterval == 0 {
		option.PartitionPollInterval = 100 * time.Millisecond
	}
	if option.PartitionPollInterval < 0 {
		return errors.New("partition poll interval must not be negative")
	}

	a := &app{
		system:       system,
		input:        stdin,
		commandInput: stdin,
		out:          stdout,
		errOut:       stderr,
		option:       option,
	}
	return a.run(ctx)
}

func (a *app) run(ctx context.Context) (returnErr error) {
	disks, err := a.listDisks(ctx)
	if err != nil {
		return err
	}
	if len(disks) == 0 {
		return errors.New("no eligible whole /dev/sdX disks found")
	}

	fmt.Fprintln(a.out, "detected whole disks:")
	writeDiskTable(a.out, disks)
	fmt.Fprint(a.out, "\nwhich whole disk do you want to reformat (and completely erase)? ")
	device, err := terminal.ReadLine(ctx, a.input)
	if err != nil {
		return fmt.Errorf("read device selection: %w", err)
	}

	before, err := a.inspectSafeDevice(ctx, device)
	if err != nil {
		return err
	}
	cryptName := filepath.Base(device)
	mountpoint := filepath.Join(a.option.HomeDir, "mnt."+cryptName)
	mapper := filepath.Join(a.option.MapperDir, cryptName)
	if err := a.assertMountpointSafe(ctx, mountpoint); err != nil {
		return err
	}
	if err := a.assertPathAbsent(mapper, "mapper name is already in use"); err != nil {
		return err
	}

	fmt.Fprintln(a.out, "\nselected disk:")
	writeDiskTable(a.out, before)
	fmt.Fprintf(a.out, "\ncrypt name: %s\n", cryptName)
	fmt.Fprintf(a.out, "mountpoint: %s\n", mountpoint)
	fmt.Fprintf(a.out, "\nWARNING: all data on %s will be permanently erased.\n", device)
	fmt.Fprintf(a.out, "Type the exact device path (%s) to confirm: ", device)
	confirmation, err := terminal.ReadLine(ctx, a.input)
	if err != nil {
		return fmt.Errorf("read confirmation: %w", err)
	}
	if confirmation != device {
		fmt.Fprintln(a.out, "\naborting; nothing was changed.")
		return ErrDeclined
	}

	after, err := a.inspectSafeDevice(ctx, device)
	if err != nil {
		return fmt.Errorf("revalidate selected device: %w", err)
	}
	if !reflect.DeepEqual(normalizeDevices(before), normalizeDevices(after)) {
		return fmt.Errorf("device identity or state changed while awaiting confirmation: %s", device)
	}
	if err := a.assertMountpointSafe(ctx, mountpoint); err != nil {
		return fmt.Errorf("revalidate mountpoint: %w", err)
	}
	if err := a.assertPathAbsent(mapper, "mapper name became active while awaiting confirmation"); err != nil {
		return err
	}

	fmt.Fprintf(a.out, "\nconfirmed and revalidated; formatting %s...\n\n", device)
	partitionInput := strings.NewReader("g\nn\n1\n\n\n\nw\n")
	if err := a.runCommand(ctx, partitionInput, "fdisk", "-w", "always", "-W", "always", device); err != nil {
		return fmt.Errorf("write partition table: %w", err)
	}

	rootBeforeWrite, found := findDevice(after, device)
	if !found {
		return fmt.Errorf("revalidated device tree lost its root: %s", device)
	}
	partition, err := a.waitForSinglePartition(ctx, device, identityOf(rootBeforeWrite))
	if err != nil {
		return err
	}

	// This must run exactly once. In particular, an open failure must never
	// cause a second luksFormat that rewrites the newly created LUKS header.
	if err := a.runCommand(
		ctx,
		a.commandInput,
		"cryptsetup",
		"-v",
		"luksFormat",
		"--type",
		"luks2",
		"--key-size=512",
		"--perf-no_read_workqueue",
		"--perf-no_write_workqueue",
		partition,
	); err != nil {
		return fmt.Errorf("format LUKS container: %w", err)
	}
	if err := a.runCommand(ctx, nil, "cryptsetup", "isLuks", partition); err != nil {
		return fmt.Errorf("verify LUKS container: %w", err)
	}

	mapperOpen := false
	mounted := false
	defer func() {
		if returnErr == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if mounted {
			if cleanupErr := a.runCommand(cleanupCtx, nil, "umount", mountpoint); cleanupErr != nil {
				fmt.Fprintf(a.errOut, "warning: failed to unmount %s during cleanup: %v\n", mountpoint, cleanupErr)
				return
			}
		}
		if mapperOpen {
			if cleanupErr := a.runCommand(cleanupCtx, nil, "cryptsetup", "close", cryptName); cleanupErr != nil {
				fmt.Fprintf(a.errOut, "warning: failed to close %s during cleanup: %v\n", cryptName, cleanupErr)
			}
		}
	}()

	for {
		err = a.runCommand(ctx, a.commandInput, "cryptsetup", "open", partition, cryptName)
		if err == nil {
			mapperOpen = true
			break
		}
		fmt.Fprintf(a.errOut, "opening LUKS container failed: %v\n", err)
		fmt.Fprint(a.out, "retry opening it? y/N: ")
		answer, readErr := terminal.ReadLine(ctx, a.input)
		if readErr != nil {
			return fmt.Errorf("read open retry response: %w", readErr)
		}
		if answer != "y" {
			return errors.New("opening LUKS container was not completed; the LUKS header was not reformatted")
		}
	}

	if err := a.assertPathPresent(mapper, "cryptsetup reported success but the mapper does not exist"); err != nil {
		return err
	}
	if err := a.runCommand(ctx, nil, "mkfs.ext4", "-m", "0", mapper); err != nil {
		return fmt.Errorf("create ext4 filesystem: %w", err)
	}
	filesystem, err := a.outputCommand(ctx, "blkid", "-p", "-s", "TYPE", "-o", "value", mapper)
	if err != nil {
		return fmt.Errorf("verify ext4 filesystem: %w", err)
	}
	if strings.TrimSpace(string(filesystem)) != "ext4" {
		return fmt.Errorf("expected ext4 on %s after formatting, got %q", mapper, strings.TrimSpace(string(filesystem)))
	}

	if err := a.system.MkdirAll(mountpoint, 0o755); err != nil {
		return fmt.Errorf("create mountpoint %s: %w", mountpoint, err)
	}
	if err := a.assertMountpointSafe(ctx, mountpoint); err != nil {
		return fmt.Errorf("validate created mountpoint: %w", err)
	}
	if err := a.runCommand(ctx, nil, "mount", mapper, mountpoint); err != nil {
		return fmt.Errorf("mount formatted disk: %w", err)
	}
	mounted = true

	source, err := a.system.Output(ctx, "findmnt", "--noheadings", "--output", "SOURCE", "--target", mountpoint)
	if err != nil {
		return fmt.Errorf("verify mounted disk: %w", err)
	}
	if strings.TrimSpace(string(source)) != mapper {
		return fmt.Errorf("mount verification returned %q for %s, expected %s", strings.TrimSpace(string(source)), mountpoint, mapper)
	}
	owner := fmt.Sprintf("%d:%d", a.option.OwnerUID, a.option.OwnerGID)
	if err := a.runCommand(ctx, nil, "chown", owner, mountpoint); err != nil {
		return fmt.Errorf("set mount ownership on %s: %w", mountpoint, err)
	}

	fmt.Fprintf(a.out, "\nformatted and mounted %s at %s\n", device, mountpoint)
	return nil
}

var ErrDeclined = errors.New("format declined")

func (a *app) listDisks(ctx context.Context) ([]blockDevice, error) {
	devices, err := a.lsblk(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("list block devices: %w", err)
	}
	eligible := devices[:0]
	for _, device := range devices {
		if a.devicePathAllowed(device.Path) && a.rootTypeAllowed(device.Type) {
			eligible = append(eligible, device)
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		return eligible[i].Path < eligible[j].Path
	})
	return eligible, nil
}

func (a *app) inspectSafeDevice(ctx context.Context, path string) ([]blockDevice, error) {
	if !a.devicePathAllowed(path) {
		return nil, fmt.Errorf("only a whole /dev/sdX disk is supported, got: %s", path)
	}
	devices, err := a.lsblk(ctx, false, path)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", path, err)
	}
	devices = normalizeDevices(devices)
	var root *blockDevice
	for index := range devices {
		if devices[index].Path == path {
			if root != nil {
				return nil, fmt.Errorf("lsblk returned the selected device more than once: %s", path)
			}
			root = &devices[index]
		}
	}
	if root == nil {
		return nil, fmt.Errorf("selected device was not returned by lsblk: %s", path)
	}
	if !a.rootTypeAllowed(root.Type) {
		return nil, fmt.Errorf("not a whole disk: %s has type %q", path, root.Type)
	}
	if root.ReadOnly {
		return nil, fmt.Errorf("device is read-only: %s", path)
	}
	if !a.option.AllowNonSCSIBlockForTesting && root.Serial == "" && root.WWN == "" {
		return nil, fmt.Errorf("device has neither a serial nor a WWN: %s", path)
	}
	for _, device := range devices {
		for _, mountpoint := range device.Mountpoints {
			if mountpoint != nil && *mountpoint != "" {
				return nil, fmt.Errorf("device or one of its children is mounted at %s: %s", *mountpoint, path)
			}
		}
		if strings.EqualFold(device.Filesystem, "swap") {
			return nil, fmt.Errorf("device contains swap: %s", path)
		}
		if device.Path != path && device.Type != "part" {
			return nil, fmt.Errorf("device has an active holder of type %q at %s: %s", device.Type, device.Path, path)
		}
	}
	return devices, nil
}

func (a *app) lsblk(ctx context.Context, nodeps bool, paths ...string) ([]blockDevice, error) {
	args := []string{"--json", "--bytes", "--paths"}
	if nodeps {
		args = append(args, "--nodeps")
	}
	args = append(args, "--output", lsblkColumns)
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	output, err := a.system.Output(ctx, "lsblk", args...)
	if err != nil {
		return nil, err
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

func normalizeDevices(devices []blockDevice) []blockDevice {
	normalized := append([]blockDevice(nil), devices...)
	for index := range normalized {
		normalized[index].Children = nil
		sort.Slice(normalized[index].Mountpoints, func(i, j int) bool {
			left := ""
			right := ""
			if normalized[index].Mountpoints[i] != nil {
				left = *normalized[index].Mountpoints[i]
			}
			if normalized[index].Mountpoints[j] != nil {
				right = *normalized[index].Mountpoints[j]
			}
			return left < right
		})
	}
	sort.Slice(normalized, func(i, j int) bool {
		return normalized[i].Path < normalized[j].Path
	})
	return normalized
}

type diskIdentity struct {
	Path       string
	KernelName string
	MajorMinor string
	Size       uint64
	Type       string
	Model      string
	Serial     string
	WWN        string
	ReadOnly   bool
}

func firstMountpoint(device blockDevice) string {
	for _, mountpoint := range device.Mountpoints {
		if mountpoint != nil && *mountpoint != "" {
			return *mountpoint
		}
	}
	return ""
}

func identityOf(device blockDevice) diskIdentity {
	return diskIdentity{
		Path:       device.Path,
		KernelName: device.KernelName,
		MajorMinor: device.MajorMinor,
		Size:       device.Size,
		Type:       device.Type,
		Model:      device.Model,
		Serial:     device.Serial,
		WWN:        device.WWN,
		ReadOnly:   device.ReadOnly,
	}
}

func findDevice(devices []blockDevice, path string) (blockDevice, bool) {
	for _, device := range devices {
		if device.Path == path {
			return device, true
		}
	}
	return blockDevice{}, false
}

func (a *app) waitForSinglePartition(ctx context.Context, device string, expectedIdentity diskIdentity) (string, error) {
	expected := device + "1"
	if a.option.AllowNonSCSIBlockForTesting && testLoopDevicePattern.MatchString(device) {
		expected = device + "p1"
	}
	for attempt := 0; attempt < a.option.PartitionPollAttempts; attempt++ {
		devices, err := a.lsblk(ctx, false, device)
		if err == nil {
			devices = normalizeDevices(devices)
			root, found := findDevice(devices, device)
			if !found {
				return "", fmt.Errorf("selected disk disappeared after partitioning: %s", device)
			}
			if currentIdentity := identityOf(root); currentIdentity != expectedIdentity {
				return "", fmt.Errorf("selected disk identity changed after partitioning: %s", device)
			}
			if mountpoint := firstMountpoint(root); mountpoint != "" {
				return "", fmt.Errorf("selected disk became mounted at %s after partitioning: %s", mountpoint, device)
			}
			var partitions []blockDevice
			unexpectedHolder := ""
			for _, candidate := range devices {
				if candidate.Path == device {
					continue
				}
				if candidate.Type != "part" {
					unexpectedHolder = candidate.Type
					break
				}
				if mountpoint := firstMountpoint(candidate); mountpoint != "" {
					return "", fmt.Errorf("new partition became mounted at %s: %s", mountpoint, candidate.Path)
				}
				partitions = append(partitions, candidate)
			}
			if unexpectedHolder != "" {
				return "", fmt.Errorf("unexpected holder %q appeared after partitioning %s", unexpectedHolder, device)
			}
			if len(partitions) == 1 && partitions[0].Path == expected {
				return expected, nil
			}
		}
		if attempt+1 < a.option.PartitionPollAttempts {
			if sleepErr := a.system.Sleep(ctx, a.option.PartitionPollInterval); sleepErr != nil {
				return "", fmt.Errorf("wait for partition %s: %w", expected, sleepErr)
			}
		}
	}
	return "", fmt.Errorf("fdisk completed but exactly one partition at %s did not appear", expected)
}

func (a *app) runCommand(ctx context.Context, stdin io.Reader, command string, args ...string) error {
	return a.system.Run(ctx, stdin, a.out, a.errOut, command, args...)
}

func (a *app) outputCommand(ctx context.Context, command string, args ...string) ([]byte, error) {
	return a.system.Output(ctx, command, args...)
}

func (a *app) assertMountpointSafe(ctx context.Context, mountpoint string) error {
	output, err := a.system.Output(ctx, "findmnt", "--json", "--output", "TARGET")
	if err != nil {
		return fmt.Errorf("inspect mount tree for %s: %w", mountpoint, err)
	}
	var document findmntDocument
	if err := json.Unmarshal(output, &document); err != nil {
		return fmt.Errorf("decode findmnt JSON for %s: %w", mountpoint, err)
	}
	if len(document.Filesystems) == 0 {
		return fmt.Errorf("findmnt returned an empty mount tree while inspecting %s", mountpoint)
	}
	mountedPath, err := mountAtOrBelow(document.Filesystems, mountpoint)
	if err != nil {
		return fmt.Errorf("validate mount tree for %s: %w", mountpoint, err)
	}
	if mountedPath != "" {
		return fmt.Errorf("something is already mounted at or below %s: %s", mountpoint, mountedPath)
	}

	info, err := a.system.Lstat(mountpoint)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect mountpoint path %s: %w", mountpoint, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("mountpoint must be absent or a real directory, not a symlink or other file: %s", mountpoint)
	}
	empty, err := a.system.DirectoryEmpty(mountpoint)
	if err != nil {
		return fmt.Errorf("inspect contents of mountpoint %s: %w", mountpoint, err)
	}
	if !empty {
		return fmt.Errorf("mountpoint directory is not empty: %s", mountpoint)
	}
	return nil
}

func mountAtOrBelow(records []mountRecord, root string) (string, error) {
	for _, record := range records {
		if !filepath.IsAbs(record.Target) || filepath.Clean(record.Target) != record.Target {
			return "", fmt.Errorf("findmnt returned an invalid target path %q", record.Target)
		}
		if record.Target == root || strings.HasPrefix(record.Target, root+string(filepath.Separator)) {
			return record.Target, nil
		}
		if target, err := mountAtOrBelow(record.Children, root); err != nil || target != "" {
			return target, err
		}
	}
	return "", nil
}

func (a *app) assertPathAbsent(path string, message string) error {
	_, err := a.system.Lstat(path)
	if err == nil {
		return fmt.Errorf("%s: %s", message, path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	return nil
}

func (a *app) assertPathPresent(path string, message string) error {
	_, err := a.system.Lstat(path)
	if err != nil {
		return fmt.Errorf("%s: %s: %w", message, path, err)
	}
	return nil
}

func (a *app) devicePathAllowed(path string) bool {
	if a.option.AllowNonSCSIBlockForTesting {
		return filepath.IsAbs(path) && filepath.Clean(path) == path
	}
	return productionDevicePattern.MatchString(path)
}

func (a *app) rootTypeAllowed(deviceType string) bool {
	if a.option.AllowNonSCSIBlockForTesting {
		return deviceType == "disk" || deviceType == "loop"
	}
	return deviceType == "disk"
}

func writeDiskTable(writer io.Writer, devices []blockDevice) {
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "PATH\tSIZE\tTYPE\tMODEL\tSERIAL\tWWN")
	for _, device := range devices {
		fmt.Fprintf(
			table,
			"%s\t%s\t%s\t%s\t%s\t%s\n",
			device.Path,
			humanSize(device.Size),
			device.Type,
			emptyAsDash(device.Model),
			emptyAsDash(device.Serial),
			emptyAsDash(device.WWN),
		)
	}
	_ = table.Flush()
}

func humanSize(size uint64) string {
	const unit = 1000
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	units := []string{"kB", "MB", "GB", "TB", "PB"}
	value := float64(size)
	unitIndex := -1
	for value >= unit && unitIndex+1 < len(units) {
		value /= unit
		unitIndex++
	}
	return fmt.Sprintf("%.2f %s", value, units[unitIndex])
}

func emptyAsDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
