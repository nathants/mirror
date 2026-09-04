package mountdisks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"text/tabwriter"
	"time"
)

var ErrDeclined = errors.New("operation declined")

type Options struct {
	ConfigPath   string
	HomeDir      string
	MapperDir    string
	OwnerUID     int
	OwnerGID     int
	ReadPassword func() ([]byte, error)
}

type app struct {
	system System
	input  io.Reader
	out    io.Writer
	errOut io.Writer
	option Options
}

func Mount(ctx context.Context, system System, stdin io.Reader, stdout, stderr io.Writer, option Options) error {
	a, err := newApp(system, stdin, stdout, stderr, option)
	if err != nil {
		return err
	}
	return a.mount(ctx)
}

func Unmount(ctx context.Context, system System, stdin io.Reader, stdout, stderr io.Writer, option Options) error {
	a, err := newApp(system, stdin, stdout, stderr, option)
	if err != nil {
		return err
	}
	return a.unmount(ctx)
}

func newApp(system System, stdin io.Reader, stdout, stderr io.Writer, option Options) (*app, error) {
	if system == nil {
		return nil, errors.New("system is required")
	}
	if stdin == nil || stdout == nil || stderr == nil {
		return nil, errors.New("stdin, stdout, and stderr are required")
	}
	if option.ConfigPath == "" || !filepath.IsAbs(option.ConfigPath) || filepath.Clean(option.ConfigPath) != option.ConfigPath {
		return nil, errors.New("config path must be a clean absolute path")
	}
	if option.HomeDir == "" || !filepath.IsAbs(option.HomeDir) || filepath.Clean(option.HomeDir) != option.HomeDir {
		return nil, errors.New("home directory must be a clean absolute path")
	}
	if option.MapperDir == "" {
		option.MapperDir = "/dev/mapper"
	}
	if !filepath.IsAbs(option.MapperDir) || filepath.Clean(option.MapperDir) != option.MapperDir {
		return nil, errors.New("mapper directory must be a clean absolute path")
	}
	if option.OwnerUID < 0 || option.OwnerGID < 0 {
		return nil, errors.New("owner UID and GID must not be negative")
	}
	return &app{system: system, input: stdin, out: stdout, errOut: stderr, option: option}, nil
}

func (a *app) mount(ctx context.Context) error {
	identifiers, plans, missing, err := a.preflight(ctx)
	if err != nil {
		return err
	}
	for _, identifier := range missing {
		fmt.Fprintf(a.errOut, "drive not found: %s\n", identifier)
	}
	var actions []diskPlan
	needsPassword := false
	for _, plan := range plans {
		if plan.MountedSource != "" {
			continue
		}
		if err := a.validateMountpoint(plan.Mountpoint, true); err != nil {
			return err
		}
		if err := a.validateMapperPresence(plan); err != nil {
			return err
		}
		actions = append(actions, plan)
		if plan.Mapper == nil {
			needsPassword = true
		}
	}
	if len(actions) == 0 {
		return errors.New("nothing to mount")
	}

	fmt.Fprintln(a.out, "going to unlock and mount:")
	writePlanTable(a.out, actions)
	if err := a.confirm("mount"); err != nil {
		return err
	}

	var password []byte
	if needsPassword {
		if a.option.ReadPassword == nil {
			return errors.New("password reader is required to open a closed mapper")
		}
		password, err = a.option.ReadPassword()
		if err != nil {
			return fmt.Errorf("read disk password: %w", err)
		}
		defer clear(password)
		if len(password) == 0 {
			return errors.New("disk password must not be empty")
		}
	}

	afterIdentifiers, afterPlans, afterMissing, err := a.preflight(ctx)
	if err != nil {
		return fmt.Errorf("revalidate mount plan: %w", err)
	}
	if !reflect.DeepEqual(identifiers, afterIdentifiers) || !reflect.DeepEqual(plans, afterPlans) || !reflect.DeepEqual(missing, afterMissing) {
		return errors.New("configured disk identity or state changed while awaiting confirmation")
	}
	for _, plan := range actions {
		if err := a.validateMountpoint(plan.Mountpoint, true); err != nil {
			return fmt.Errorf("revalidate mountpoint: %w", err)
		}
		if err := a.validateMapperPresence(plan); err != nil {
			return fmt.Errorf("revalidate mapper: %w", err)
		}
	}

	type appliedState struct {
		plan    diskPlan
		opened  bool
		mounted bool
	}
	applied := make([]appliedState, 0, len(actions))
	succeeded := false
	defer func() {
		if succeeded {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for index := len(applied) - 1; index >= 0; index-- {
			state := applied[index]
			if state.mounted {
				if cleanupErr := a.runCommand(cleanupCtx, nil, "umount", state.plan.Mountpoint); cleanupErr != nil {
					fmt.Fprintf(a.errOut, "warning: failed to roll back mount %s: %v\n", state.plan.Mountpoint, cleanupErr)
					continue
				}
			}
			if state.opened {
				if cleanupErr := a.runCommand(cleanupCtx, nil, "cryptsetup", "close", state.plan.CryptName); cleanupErr != nil {
					fmt.Fprintf(a.errOut, "warning: failed to roll back mapper %s: %v\n", state.plan.MapperPath, cleanupErr)
				}
			}
		}
	}()

	for _, plan := range actions {
		state := appliedState{plan: plan}
		applied = append(applied, state)
		current := &applied[len(applied)-1]
		if plan.Mapper == nil {
			if err := a.runCommand(ctx, bytes.NewReader(password), "cryptsetup", "open", plan.Partition.Path, plan.CryptName, "--key-file", "-"); err != nil {
				return fmt.Errorf("open %s: %w", plan.Partition.Path, err)
			}
			current.opened = true
			if _, err := a.system.Lstat(plan.MapperPath); err != nil {
				return fmt.Errorf("cryptsetup reported success but mapper is absent at %s: %w", plan.MapperPath, err)
			}
			filesystem, err := a.outputCommand(ctx, "blkid", "-p", "-s", "TYPE", "-o", "value", plan.MapperPath)
			if err != nil {
				return fmt.Errorf("inspect filesystem on %s: %w", plan.MapperPath, err)
			}
			if strings.TrimSpace(string(filesystem)) != "ext4" {
				return fmt.Errorf("expected ext4 on %s, got %q", plan.MapperPath, strings.TrimSpace(string(filesystem)))
			}
		}
		if err := a.system.MkdirAll(plan.Mountpoint, 0o755); err != nil {
			return fmt.Errorf("create mountpoint %s: %w", plan.Mountpoint, err)
		}
		if err := a.validateMountpoint(plan.Mountpoint, true); err != nil {
			return err
		}
		if err := a.runCommand(ctx, nil, "mount", plan.MapperPath, plan.Mountpoint); err != nil {
			return fmt.Errorf("mount %s: %w", plan.MapperPath, err)
		}
		current.mounted = true
		source, err := a.system.Output(ctx, "findmnt", "--noheadings", "--output", "SOURCE", "--target", plan.Mountpoint)
		if err != nil {
			return fmt.Errorf("verify mountpoint %s: %w", plan.Mountpoint, err)
		}
		if strings.TrimSpace(string(source)) != plan.MapperPath {
			return fmt.Errorf("mountpoint %s is backed by %q, expected %s", plan.Mountpoint, strings.TrimSpace(string(source)), plan.MapperPath)
		}
		owner := fmt.Sprintf("%d:%d", a.option.OwnerUID, a.option.OwnerGID)
		if err := a.runCommand(ctx, nil, "chown", owner, plan.Mountpoint); err != nil {
			return fmt.Errorf("set mount ownership on %s: %w", plan.Mountpoint, err)
		}
		fmt.Fprintf(a.out, "mounted %s at %s\n", plan.Partition.Path, plan.Mountpoint)
	}
	succeeded = true
	return nil
}

func (a *app) unmount(ctx context.Context) error {
	identifiers, plans, missing, err := a.preflight(ctx)
	if err != nil {
		return err
	}
	for _, identifier := range missing {
		fmt.Fprintf(a.errOut, "drive not found: %s\n", identifier)
	}
	var actions []diskPlan
	for _, plan := range plans {
		if plan.Mapper == nil {
			continue
		}
		if err := a.validateMapperPresence(plan); err != nil {
			return err
		}
		if plan.MountedSource != "" {
			if err := a.validateMountpoint(plan.Mountpoint, false); err != nil {
				return err
			}
		}
		actions = append(actions, plan)
	}
	if len(actions) == 0 {
		return errors.New("nothing to unmount")
	}

	fmt.Fprintln(a.out, "going to unmount and close:")
	writePlanTable(a.out, actions)
	if err := a.confirm("unmount"); err != nil {
		return err
	}
	afterIdentifiers, afterPlans, afterMissing, err := a.preflight(ctx)
	if err != nil {
		return fmt.Errorf("revalidate unmount plan: %w", err)
	}
	if !reflect.DeepEqual(identifiers, afterIdentifiers) || !reflect.DeepEqual(plans, afterPlans) || !reflect.DeepEqual(missing, afterMissing) {
		return errors.New("configured disk identity or state changed while awaiting confirmation")
	}

	for _, plan := range actions {
		if plan.MountedSource != "" {
			if err := a.validateMountpoint(plan.Mountpoint, false); err != nil {
				return fmt.Errorf("revalidate mountpoint: %w", err)
			}
			if err := a.runCommand(ctx, nil, "umount", plan.Mountpoint); err != nil {
				return fmt.Errorf("unmount %s: %w", plan.Mountpoint, err)
			}
		}
		if err := a.runCommand(ctx, nil, "cryptsetup", "close", plan.CryptName); err != nil {
			return fmt.Errorf("close %s: %w", plan.MapperPath, err)
		}
		if _, err := a.system.Lstat(plan.MapperPath); err == nil {
			return fmt.Errorf("cryptsetup reported success but mapper still exists: %s", plan.MapperPath)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect closed mapper %s: %w", plan.MapperPath, err)
		}
		fmt.Fprintf(a.out, "closed %s\n", plan.MapperPath)
	}
	return nil
}

func (a *app) preflight(ctx context.Context) ([]string, []diskPlan, []string, error) {
	identifiers, err := loadConfig(a.option.ConfigPath)
	if err != nil {
		return nil, nil, nil, err
	}
	plans, missing, err := inspectPlans(ctx, a.system, identifiers, a.option.HomeDir, a.option.MapperDir)
	if err != nil {
		return nil, nil, nil, err
	}
	return identifiers, plans, missing, nil
}

func (a *app) validateMapperPresence(plan diskPlan) error {
	_, err := a.system.Lstat(plan.MapperPath)
	if plan.Mapper == nil {
		if err == nil {
			return fmt.Errorf("mapper exists but lsblk did not report it: %s", plan.MapperPath)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect mapper %s: %w", plan.MapperPath, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("lsblk reported mapper but its path is unavailable at %s: %w", plan.MapperPath, err)
	}
	return nil
}

func (a *app) validateMountpoint(path string, requireEmpty bool) error {
	info, err := a.system.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect mountpoint %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("mountpoint must be a real directory, not a symlink or other file: %s", path)
	}
	if !requireEmpty {
		return nil
	}
	empty, err := a.system.DirectoryEmpty(path)
	if err != nil {
		return fmt.Errorf("inspect contents of mountpoint %s: %w", path, err)
	}
	if !empty {
		return fmt.Errorf("mountpoint directory is not empty: %s", path)
	}
	return nil
}

func (a *app) confirm(action string) error {
	fmt.Fprintf(a.out, "type %q to confirm: ", action)
	answer, err := readLine(a.input)
	if err != nil {
		return fmt.Errorf("read confirmation: %w", err)
	}
	if answer != action {
		fmt.Fprintln(a.out, "aborting; nothing was changed.")
		return ErrDeclined
	}
	return nil
}

func (a *app) outputCommand(ctx context.Context, command string, args ...string) ([]byte, error) {
	return a.system.Output(ctx, command, args...)
}

func (a *app) runCommand(ctx context.Context, stdin io.Reader, command string, args ...string) error {
	return a.system.Run(ctx, stdin, a.out, a.errOut, command, args...)
}

func readLine(reader io.Reader) (string, error) {
	var line []byte
	var one [1]byte
	for {
		count, err := reader.Read(one[:])
		if count == 1 {
			if one[0] == '\n' {
				return strings.TrimSuffix(string(line), "\r"), nil
			}
			line = append(line, one[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return strings.TrimSuffix(string(line), "\r"), nil
			}
			return "", err
		}
		if count == 0 {
			return "", io.ErrNoProgress
		}
	}
}

func writePlanTable(writer io.Writer, plans []diskPlan) {
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "DISK\tPARTITION\tMODEL\tSERIAL\tWWN\tSTATE\tMOUNTPOINT")
	for _, plan := range plans {
		state := "closed"
		if plan.Mapper != nil {
			state = "open"
		}
		if plan.MountedSource != "" {
			state = "mounted"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			plan.Disk.Path,
			plan.Partition.Path,
			emptyAsDash(plan.Disk.Model),
			emptyAsDash(plan.Disk.Serial),
			emptyAsDash(plan.Disk.WWN),
			state,
			plan.Mountpoint,
		)
	}
	_ = table.Flush()
}

func emptyAsDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
