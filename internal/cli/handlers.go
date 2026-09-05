package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mirror/internal/actionlog"
	"mirror/internal/ensurecopies"
	"mirror/internal/formatdisk"
	"mirror/internal/mountdisks"
	"mirror/internal/operationlock"
	"mirror/internal/repaircopies"
	"mirror/internal/system"
	"mirror/internal/terminal"
	"mirror/lib"
)

// copyDependencies keeps mounted-state discovery and controlling-terminal
// confirmation at the CLI boundary, shared by the real command workflows.
type copyDependencies struct {
	snapshot func(context.Context, string) (mountdisks.Snapshot, error)
	confirm  func(context.Context, io.Writer, string) error
}

func commandHandlers(stdin io.Reader, stdout, stderr io.Writer, copies copyDependencies) handlers {
	return handlers{
		format: func(ctx context.Context, caller callerIdentity) error {
			return withLoggedDiskOperation(ctx, caller, "format", stderr, func(os *actionlog.System) error {
				err := formatdisk.Run(ctx, os, stdin, stdout, stderr, formatdisk.Options{HomeDir: caller.HomeDir, OwnerUID: caller.UID, OwnerGID: caller.GID})
				return declinedAsSilent(err, formatdisk.ErrDeclined)
			})
		},
		mount: func(ctx context.Context, caller callerIdentity) error {
			return withLoggedDiskOperation(ctx, caller, "mount", stderr, func(os *actionlog.System) error {
				err := mountdisks.Mount(ctx, os, stdin, stdout, stderr, mountdisks.Options{
					ConfigPath: filepath.Join(caller.HomeDir, ".mirror", "disks"), HomeDir: caller.HomeDir, OwnerUID: caller.UID, OwnerGID: caller.GID,
					ReadPassword: func() ([]byte, error) { return terminal.ReadPassword(ctx, stdout, "disk password: ") },
				})
				return declinedAsSilent(err, mountdisks.ErrDeclined)
			})
		},
		unmount: func(ctx context.Context, caller callerIdentity) error {
			return withLoggedDiskOperation(ctx, caller, "unmount", stderr, func(os *actionlog.System) error {
				err := mountdisks.Unmount(ctx, os, stdin, stdout, stderr, mountdisks.Options{ConfigPath: filepath.Join(caller.HomeDir, ".mirror", "disks"), HomeDir: caller.HomeDir, OwnerUID: caller.UID, OwnerGID: caller.GID})
				return declinedAsSilent(err, mountdisks.ErrDeclined)
			})
		},
		ensureCopies: func(ctx context.Context, preview bool) error {
			return withOwnLock(ctx, func(home string) error {
				return ensurecopies.Run(ctx, preview, ensurecopies.Options{
					Snapshot: func(ctx context.Context) (mountdisks.Snapshot, error) { return copies.snapshot(ctx, home) },
					Output:   stdout,
				})
			})
		},
		repairCopies: func(ctx context.Context, preview, resume bool) error {
			return withOwnLock(ctx, func(home string) error {
				return repaircopies.Run(ctx, home, preview, resume, repaircopies.Options{
					Snapshot: func(ctx context.Context) (mountdisks.Snapshot, error) { return copies.snapshot(ctx, home) },
					Confirm: func(ctx context.Context) error {
						return copies.confirm(ctx, stdout, "going to repair the safe files listed above; unresolved files will be left untouched.")
					},
					Output: stdout,
				})
			})
		},
		lock: func(ctx context.Context) error {
			return withOwnLock(ctx, func(home string) error { return lib.LockDirs(ctx, home, true) })
		},
		unlock: func(ctx context.Context) error {
			return withOwnLock(ctx, func(home string) error { return lib.LockDirs(ctx, home, false) })
		},
	}
}
func withLoggedDiskOperation(ctx context.Context, caller callerIdentity, operation string, output io.Writer, run func(*actionlog.System) error) error {
	return withLock(ctx, caller, func() error {
		log, err := actionlog.Open(caller.HomeDir, operation, caller.UID, caller.GID, output)
		if err != nil {
			return err
		}
		observed := &actionlog.System{Backend: system.Exec{}, Log: log, Context: ctx}
		result := run(observed)
		logErr := errors.Join(observed.Finish(errors.Join(result, ctx.Err())), log.Close())
		if logErr != nil {
			// Do not hide logging failures behind a normally silent decline.
			return fmt.Errorf("operation result: %v; action logging failed: %w", result, logErr)
		}
		return result
	})
}

func withOwnLock(ctx context.Context, run func(string) error) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return withLock(ctx, callerIdentity{HomeDir: home, UID: os.Geteuid(), GID: os.Getegid()}, func() error { return run(home) })
}
func withLock(ctx context.Context, caller callerIdentity, run func() error) (returnErr error) {
	lock, err := operationlock.Acquire(ctx, caller.HomeDir, caller.UID, caller.GID)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, lock.Close()) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	returnErr = run()
	return errors.Join(returnErr, ctx.Err())
}
func declinedAsSilent(err, declined error) error {
	if errors.Is(err, declined) {
		return fmt.Errorf("%w: %v", errDeclined, err)
	}
	return err
}
