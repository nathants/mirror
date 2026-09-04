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
	"mirror/internal/repaircopies"
	"mirror/internal/system"
	"mirror/internal/terminal"
	"mirror/lib"
)

func productionHandlers(stdin io.Reader, stdout, stderr io.Writer) handlers {
	return handlers{
		format: func(ctx context.Context, caller callerIdentity) error {
			err := formatdisk.Run(
				ctx,
				system.Exec{},
				stdin,
				stdout,
				stderr,
				formatdisk.Options{
					HomeDir:  caller.HomeDir,
					OwnerUID: caller.UID,
					OwnerGID: caller.GID,
				},
			)
			return declinedAsSilent(err, formatdisk.ErrDeclined)
		},
		mount: func(ctx context.Context, caller callerIdentity) error {
			err := mountdisks.Mount(ctx, system.Exec{}, stdin, stdout, stderr, mountdisks.Options{
				ConfigPath: filepath.Join(caller.HomeDir, ".mirror", "disks"),
				HomeDir:    caller.HomeDir,
				OwnerUID:   caller.UID,
				OwnerGID:   caller.GID,
				ReadPassword: func() ([]byte, error) {
					return terminal.ReadPassword(stdout, "disk password: ")
				},
			})
			return declinedAsSilent(err, mountdisks.ErrDeclined)
		},
		unmount: func(ctx context.Context, caller callerIdentity) error {
			err := mountdisks.Unmount(ctx, system.Exec{}, stdin, stdout, stderr, mountdisks.Options{
				ConfigPath: filepath.Join(caller.HomeDir, ".mirror", "disks"),
				HomeDir:    caller.HomeDir,
				OwnerUID:   caller.UID,
				OwnerGID:   caller.GID,
			})
			return declinedAsSilent(err, mountdisks.ErrDeclined)
		},
		ensureCopies: ensurecopies.Run,
		repairCopies: func(preview, resume bool) error {
			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("determine home directory: %w", err)
			}
			return repaircopies.Run(homeDir, preview, resume)
		},
		lock: func() error {
			lib.LockDirs(true)
			return nil
		},
		unlock: func() error {
			lib.LockDirs(false)
			return nil
		},
	}
}

func declinedAsSilent(err, declined error) error {
	if errors.Is(err, declined) {
		return fmt.Errorf("%w: %v", errDeclined, err)
	}
	return err
}
