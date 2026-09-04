package rootfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"
)

// WalkPinnedDirs visits user directories in postorder on the original root
// descriptor. It intentionally survives loss of the root's pathname so guard
// cleanup can relock that filesystem, never the path's replacement. Normal
// mutations must additionally call Check. All opens retain no-symlink/no-mount
// confinement; private state and lost+found are excluded. The callback borrows
// each directory descriptor only for the call; it must not close or retain it.
func (r *Root) WalkPinnedDirs(ctx context.Context, visit func(string, *os.File) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.checkInternal(); err != nil {
		return err
	}
	var walk func(string) error
	walk = func(path string) (returnErr error) {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir, err := r.openFile(path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		defer func() { returnErr = errors.Join(returnErr, dir.Close()) }()
		entries, err := dir.ReadDir(-1)
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		var failures []error
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(failures, err)...)
			}
			rel := filepath.Join(path, entry.Name())
			if rel == Internal || rel == "lost+found" || !entry.IsDir() {
				continue
			}
			if err := walk(rel); err != nil {
				failures = append(failures, err)
			}
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		// Keep restoring other directories after a local failure. The caller
		// still receives every error, and cancellation always stops mutation.
		return errors.Join(append(failures, visit(path, dir))...)
	}
	return walk(".")
}
