package lib

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mirror/internal/mountdisks"
	"mirror/internal/rootfs"
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

func LockDirs(ctx context.Context, home string, locked bool) error {
	state, err := MountedMirrorState(ctx, home)
	if err != nil {
		return err
	}
	return LockRoots(ctx, state.Roots, locked)
}

// LockRoots applies the accidental-write guard to fixed, revalidated roots.
// Quarantine and temporary state remain private and are never walked or chmodded.
func LockRoots(ctx context.Context, paths []string, locked bool) error {
	var failures []error
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err := lockRoot(ctx, path, locked); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
func lockRoot(ctx context.Context, path string, locked bool) error {
	root, err := rootfs.Open(path)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.CheckInternal(); err != nil {
		return err
	}
	mode := os.FileMode(0744)
	if locked {
		mode = 0544
	}
	var walk func(string) error
	walk = func(dir string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := root.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			rel := filepath.Join(dir, entry.Name())
			if rel == rootfs.Internal || rel == "lost+found" {
				continue
			}
			if entry.IsDir() {
				if err := walk(rel); err != nil {
					return err
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := root.ChmodDir(dir, mode); err != nil {
			return fmt.Errorf("set directory guard on %s: %w", filepath.Join(path, dir), err)
		}
		return nil
	}
	return walk(".")
}
