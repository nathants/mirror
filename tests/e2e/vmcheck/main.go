package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

const targetBytes = uint64(1073741824)

var targetPattern = regexp.MustCompile(`^/dev/sd[a-z]+$`)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "target" {
		path, err := findTarget()
		if err != nil {
			fail(err.Error())
		}
		fmt.Println(path)
		return
	}
	if len(os.Args) != 3 || !targetPattern.MatchString(os.Args[2]) {
		fail("usage: vmcheck target | vmcheck preflight|postcheck|configure|postunmount|cleanup /dev/sdX")
	}
	targetDisk := os.Args[2]
	var err error
	switch os.Args[1] {
	case "preflight":
		err = preflight(targetDisk)
	case "postcheck":
		err = postcheck(targetDisk)
	case "configure":
		err = configure(targetDisk)
	case "postunmount":
		err = postunmount(targetDisk)
	case "cleanup":
		err = cleanup(targetDisk)
	default:
		err = fmt.Errorf("unknown action %q", os.Args[1])
	}
	if err != nil {
		fail(err.Error())
	}
}

type targetDocument struct {
	BlockDevices []struct {
		Path     string `json:"path"`
		Size     uint64 `json:"size"`
		Type     string `json:"type"`
		Serial   string `json:"serial"`
		ReadOnly bool   `json:"ro"`
	} `json:"blockdevices"`
}

func findTarget() (string, error) {
	data, err := output("lsblk", "--json", "--bytes", "--paths", "--nodeps", "--output", "PATH,SIZE,TYPE,SERIAL,RO")
	if err != nil {
		return "", err
	}
	var document targetDocument
	if err := json.Unmarshal([]byte(data), &document); err != nil {
		return "", fmt.Errorf("decode lsblk target inventory: %w", err)
	}
	var matches []string
	for _, device := range document.BlockDevices {
		if targetPattern.MatchString(device.Path) && device.Size == targetBytes && device.Type == "disk" && device.Serial != "" && !device.ReadOnly {
			matches = append(matches, device.Path)
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("expected exactly one writable 1 GiB /dev/sdX disk with a serial, found %v", matches)
	}
	return matches[0], nil
}

func preflight(targetDisk string) error {
	serial, err := output("lsblk", "-dn", "-o", "SERIAL", targetDisk)
	if err != nil {
		return err
	}
	if serial == "" {
		return fmt.Errorf("target data disk has no stable serial: %s", targetDisk)
	}
	checks := []struct {
		command string
		args    []string
		want    string
	}{
		{command: "blockdev", args: []string{"--getsize64", targetDisk}, want: strconv.FormatUint(targetBytes, 10)},
		{command: "lsblk", args: []string{"-dn", "-o", "TYPE", targetDisk}, want: "disk"},
	}
	for _, check := range checks {
		got, err := output(check.command, check.args...)
		if err != nil {
			return err
		}
		if got != check.want {
			return fmt.Errorf("%s %v returned %q, want %q", check.command, check.args, got, check.want)
		}
	}
	paths, err := output("lsblk", "-nr", "-o", "PATH", targetDisk)
	if err != nil {
		return err
	}
	pathFields := strings.Fields(paths)
	if len(pathFields) != 1 || pathFields[0] != targetDisk {
		return fmt.Errorf("target was not a childless whole disk: %q", paths)
	}
	for _, path := range []string{"/tmp/mirror", "/tmp/vmcheck"} {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.Mode()&0o111 == 0 {
			return fmt.Errorf("test payload is not executable: %s", path)
		}
	}
	fmt.Println("__DEVICE_SAFE__")
	return nil
}

func postcheck(targetDisk string) error {
	name := filepath.Base(targetDisk)
	mapperPath := filepath.Join("/dev/mapper", name)
	mountpoint := filepath.Join("/home/arch", "mnt."+name)
	checks := []struct {
		command string
		args    []string
		want    string
	}{
		{command: "lsblk", args: []string{"-dn", "-o", "PTTYPE", targetDisk}, want: "gpt"},
		{command: "lsblk", args: []string{"-dn", "-o", "FSTYPE", targetDisk + "1"}, want: "crypto_LUKS"},
		{command: "blkid", args: []string{"-p", "-s", "TYPE", "-o", "value", mapperPath}, want: "ext4"},
		{command: "findmnt", args: []string{"-n", "-o", "SOURCE", "--target", mountpoint}, want: mapperPath},
	}
	for _, check := range checks {
		got, err := output(check.command, check.args...)
		if err != nil {
			return err
		}
		if got != check.want {
			return fmt.Errorf("%s %v returned %q, want %q", check.command, check.args, got, check.want)
		}
	}
	luksDump, err := output("cryptsetup", "luksDump", targetDisk+"1")
	if err != nil {
		return err
	}
	version := ""
	for _, line := range strings.Split(luksDump, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "Version:" {
			version = fields[1]
			break
		}
	}
	if version != "2" {
		return fmt.Errorf("LUKS version = %q, want 2", version)
	}
	filesystemState, err := output("tune2fs", "-l", mapperPath)
	if err != nil {
		return err
	}
	reservedBlocks := ""
	for _, line := range strings.Split(filesystemState, "\n") {
		if strings.HasPrefix(line, "Reserved block count:") {
			reservedBlocks = strings.TrimSpace(strings.TrimPrefix(line, "Reserved block count:"))
			break
		}
	}
	if reservedBlocks != "0" {
		return fmt.Errorf("ext4 reserved block count = %q, want 0", reservedBlocks)
	}

	probePath := mountpoint + "/probe"
	file, err := os.OpenFile(probePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString("e2e-payload\n"); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	data, err := os.ReadFile(probePath)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, []byte("e2e-payload\n")) {
		return fmt.Errorf("probe bytes = %q", data)
	}
	syscall.Sync()
	fmt.Println("__E2E_PASS__")
	return nil
}

func configure(targetDisk string) error {
	identifier, err := output("lsblk", "-dn", "-o", "PTUUID", targetDisk)
	if err != nil {
		return err
	}
	if !regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(identifier) {
		return fmt.Errorf("formatted disk has invalid PTUUID %q", identifier)
	}
	account, err := user.Lookup("arch")
	if err != nil {
		return fmt.Errorf("look up guest user: %w", err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return fmt.Errorf("parse guest UID: %w", err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return fmt.Errorf("parse guest GID: %w", err)
	}
	stateDir := filepath.Join(account.HomeDir, ".mirror")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return err
	}
	if err := os.Chown(stateDir, uid, gid); err != nil {
		return err
	}
	configPath := filepath.Join(stateDir, "disks")
	if err := os.WriteFile(configPath, []byte(identifier+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(configPath, 0o600); err != nil {
		return err
	}
	if err := os.Chown(configPath, uid, gid); err != nil {
		return err
	}
	fmt.Println("__CONFIGURED__")
	return nil
}

func postunmount(targetDisk string) error {
	name := filepath.Base(targetDisk)
	mapperPath := filepath.Join("/dev/mapper", name)
	if _, err := os.Lstat(mapperPath); err == nil {
		return fmt.Errorf("mapper still exists after unmount: %s", mapperPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	mountpoint := filepath.Join("/home/arch", "mnt."+name)
	command := exec.Command("findmnt", "--noheadings", "--mountpoint", mountpoint)
	if data, err := command.CombinedOutput(); err == nil {
		return fmt.Errorf("mountpoint remains mounted after unmount: %s: %s", mountpoint, strings.TrimSpace(string(data)))
	} else {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
			return fmt.Errorf("inspect unmounted path: %w: %s", err, strings.TrimSpace(string(data)))
		}
	}
	filesystem, err := output("lsblk", "-dn", "-o", "FSTYPE", targetDisk+"1")
	if err != nil {
		return err
	}
	if filesystem != "crypto_LUKS" {
		return fmt.Errorf("formatted partition type changed after unmount: %q", filesystem)
	}
	fmt.Println("__UNMOUNTED__")
	return nil
}

func cleanup(targetDisk string) error {
	name := filepath.Base(targetDisk)
	mountpoint := filepath.Join("/home/arch", "mnt."+name)
	var failures []error
	if err := run("umount", mountpoint); err != nil {
		failures = append(failures, err)
	}
	if err := run("cryptsetup", "close", name); err != nil {
		failures = append(failures, err)
	}
	if len(failures) != 0 {
		return errors.Join(failures...)
	}
	fmt.Println("__CLEANUP_READY__")
	return nil
}

func output(name string, args ...string) (string, error) {
	command := exec.Command(name, args...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	data, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %v failed: %w: %s", name, args, err, strings.TrimSpace(string(data)))
	}
	return strings.TrimSpace(string(data)), nil
}

func run(name string, args ...string) error {
	_, err := output(name, args...)
	return err
}

func fail(message string) {
	fmt.Printf("__VM_CHECK_FAIL__ %s\n", strconv.Quote(message))
	os.Exit(1)
}
