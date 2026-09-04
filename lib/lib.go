package lib

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"time"

	"mirror/internal/mountdisks"
	"mirror/internal/system"
)

// MountedMirrorState resolves only configured disks using the caller's home.
func MountedMirrorState(ctx context.Context, home string) (mountdisks.Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	state, err := mountdisks.MountedRoots(ctx, system.Exec{}, filepath.Join(home, ".mirror", "disks"), home, "/dev/mapper")
	if err != nil {
		return state, err
	}
	if len(state.Roots) == 0 {
		return state, errors.New("no configured mirror filesystems are mounted")
	}
	return state, ctx.Err()
}

func LockDirs(ctx context.Context, home string, locked bool) (returnErr error) {
	state, err := MountedMirrorState(ctx, home)
	if err != nil {
		return err
	}
	guard, err := OpenDirectoryGuard(ctx, state.Roots)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, guard.Close()) }()
	// Inventory again after pinning descriptors, before any chmod.
	current, err := MountedMirrorState(ctx, home)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, state) {
		return errors.New("mounted mirror state changed before directory guard operation")
	}
	return guard.SetLocked(ctx, locked)
}
