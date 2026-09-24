package lib

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mirror/internal/rootfs"
)

// DirectoryGuard borrows the operation's root descriptors through cleanup.
// All roots retain identity checks; only destinations receive chmods. The owner
// must keep the set open until Relock returns, even after root pathname loss.
type DirectoryGuard struct {
	roots        *rootfs.Set
	destinations []*rootfs.Root
}

// NewDirectoryGuard selects the subset whose directory modes may change.
// Empty destinations means no chmods, not all roots.
func NewDirectoryGuard(ctx context.Context, roots *rootfs.Set, destinations []string) (*DirectoryGuard, error) {
	if err := roots.Check(ctx); err != nil {
		return nil, err
	}
	guard := &DirectoryGuard{roots: roots}
	selected := make(map[string]bool)
	for _, path := range destinations {
		root := roots.Get(path)
		if root == nil {
			return nil, fmt.Errorf("directory guard destination is not a mirror root: %s", path)
		}
		if !selected[path] {
			guard.destinations = append(guard.destinations, root)
			selected[path] = true
		}
	}
	return guard, nil
}

func (g *DirectoryGuard) Check(ctx context.Context) error {
	return g.roots.Check(ctx)
}

// SetLocked is for normal lock/unlock operations. Revalidate every root before
// any mutation, and each pinned directory's root immediately before chmod.
func (g *DirectoryGuard) SetLocked(ctx context.Context, locked bool) error {
	if err := g.Check(ctx); err != nil {
		return err
	}
	return errors.Join(g.setLocked(ctx, locked, false), g.Check(ctx))
}

// Relock restores destination modes on the originally opened filesystems even
// if their mount paths disappeared. Drift of any root is still reported, but
// never redirects cleanup or prevents an attempt to relock all destinations.
func (g *DirectoryGuard) Relock(ctx context.Context) error {
	return errors.Join(g.Check(ctx), g.setLocked(ctx, true, true), g.Check(ctx))
}

func (g *DirectoryGuard) setLocked(ctx context.Context, locked, cleanup bool) error {
	mode := os.FileMode(0744)
	if locked {
		mode = 0544
	}
	var failures []error
	for _, root := range g.destinations {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		err := root.WalkPinnedDirs(ctx, func(path string, dir *os.File) error {
			if !cleanup {
				if err := root.Check(); err != nil {
					return err
				}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := dir.Chmod(mode); err != nil {
				return fmt.Errorf("set directory guard on original %s: %w", filepath.Join(root.Path(), path), err)
			}
			info, err := dir.Stat()
			if err != nil {
				return err
			}
			if info.Mode().Perm() != mode {
				return fmt.Errorf("directory guard verification failed on original %s: mode %o, want %o", filepath.Join(root.Path(), path), info.Mode().Perm(), mode)
			}
			return nil
		})
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// WithWriteDisk opens one destination's write window, including independently
// bounded cleanup on cancellation or a partial unlock. Callers serialize windows
// and stop on any returned error.
// With multiple mirrors, every user directory must start locked; source-only
// modes are never changed to manufacture a protected reader. A single mounted
// mirror can still ingest or repair without a protected second disk.
func WithWriteDisk(ctx context.Context, roots *rootfs.Set, destination string, write func() error) (returnErr error) {
	guard, err := NewDirectoryGuard(ctx, roots, []string{destination})
	if err != nil {
		return err
	}
	if err := CheckWriteInputs(ctx, roots); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		returnErr = errors.Join(returnErr, guard.Relock(cleanup))
	}()
	if err := guard.SetLocked(ctx, false); err != nil {
		return err
	}
	return write()
}

// CheckWriteInputs enforces the locked-directory prerequisite for non-preview
// copy/repair runs before scanning or validating payloads. Single-disk runs need
// no protected peer. WithWriteDisk repeats this check before each unlock.
func CheckWriteInputs(ctx context.Context, roots *rootfs.Set) error {
	if err := roots.Check(ctx); err != nil {
		return err
	}
	paths := roots.Paths()
	if len(paths) == 1 {
		return ctx.Err()
	}
	for _, path := range paths {
		root := roots.Get(path)
		if err := root.WalkPinnedDirs(ctx, func(rel string, dir *os.File) error {
			info, err := dir.Stat()
			if err != nil {
				return err
			}
			if info.Mode().Perm()&0222 != 0 {
				return fmt.Errorf("mirror directory is writable: %s; run mirror lock before multi-disk copy/repair", filepath.Join(path, rel))
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return roots.Check(ctx)
}
