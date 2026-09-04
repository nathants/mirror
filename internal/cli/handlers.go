package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mirror/internal/ensurecopies"
	"mirror/internal/formatdisk"
	"mirror/internal/mountdisks"
	"mirror/internal/operationlock"
	"mirror/internal/repaircopies"
	"mirror/internal/system"
	"mirror/internal/terminal"
	"mirror/lib"
)

func productionHandlers(stdin io.Reader, stdout, stderr io.Writer) handlers {
	return handlers{
		format: func(ctx context.Context, caller callerIdentity) error {
			return withLock(ctx, caller, func() error {
				err := formatdisk.Run(ctx, system.Exec{}, stdin, stdout, stderr, formatdisk.Options{HomeDir: caller.HomeDir, OwnerUID: caller.UID, OwnerGID: caller.GID})
				return declinedAsSilent(err, formatdisk.ErrDeclined)
			})
		},
		mount: func(ctx context.Context, caller callerIdentity) error {
			return withLock(ctx, caller, func() error {
				err := mountdisks.Mount(ctx, system.Exec{}, stdin, stdout, stderr, mountdisks.Options{
					ConfigPath: filepath.Join(caller.HomeDir, ".mirror", "disks"), HomeDir: caller.HomeDir, OwnerUID: caller.UID, OwnerGID: caller.GID,
					ReadPassword: func() ([]byte, error) { return terminal.ReadPassword(ctx, stdout, "disk password: ") },
				})
				return declinedAsSilent(err, mountdisks.ErrDeclined)
			})
		},
		unmount: func(ctx context.Context, caller callerIdentity) error {
			return withLock(ctx, caller, func() error {
				err := mountdisks.Unmount(ctx, system.Exec{}, stdin, stdout, stderr, mountdisks.Options{ConfigPath: filepath.Join(caller.HomeDir, ".mirror", "disks"), HomeDir: caller.HomeDir, OwnerUID: caller.UID, OwnerGID: caller.GID})
				return declinedAsSilent(err, mountdisks.ErrDeclined)
			})
		},
		ensureCopies: func(ctx context.Context, preview bool) error {
			return withOwnLock(ctx, func(home string) error { return ensurecopies.Run(ctx, home, preview) })
		},
		repairCopies: func(ctx context.Context, preview, resume bool) error {
			return withOwnLock(ctx, func(home string) error { return repaircopies.Run(ctx, home, preview, resume) })
		},
		lock: func(ctx context.Context) error {
			return withOwnLock(ctx, func(home string) error { return lib.LockDirs(ctx, home, true) })
		},
		unlock: func(ctx context.Context) error {
			return withOwnLock(ctx, func(home string) error { return lib.LockDirs(ctx, home, false) })
		},
	}
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
