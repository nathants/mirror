package lib

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"mirror/internal/rootfs"
)

// DirectoryGuard owns one descriptor per mirror root from preflight through
// cleanup. Directory traversal uses the original filesystems, not reopened root
// paths; only normal mutations require those paths to remain attached too.
type DirectoryGuard struct {
	roots []*rootfs.Root
}

func OpenDirectoryGuard(ctx context.Context, paths []string) (*DirectoryGuard, error) {
	if len(paths) == 0 {
		return nil, errors.New("directory guard requires mirror roots")
	}
	guard := &DirectoryGuard{}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, guard.Close())
		}
		root, err := rootfs.Open(path)
		if err != nil {
			return nil, errors.Join(err, guard.Close())
		}
		guard.roots = append(guard.roots, root)
		if err := root.CheckInternal(); err != nil {
			return nil, errors.Join(err, guard.Close())
		}
	}
	return guard, nil
}

func (g *DirectoryGuard) Close() error {
	var failures []error
	for _, root := range g.roots {
		failures = append(failures, root.Close())
	}
	g.roots = nil
	return errors.Join(failures...)
}

func (g *DirectoryGuard) Check(ctx context.Context) error {
	if len(g.roots) == 0 {
		return os.ErrClosed
	}
	for _, root := range g.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := root.Check(); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// SetLocked is for normal lock/unlock operations. Revalidate every root before
// any mutation, and each pinned directory's root immediately before chmod.
func (g *DirectoryGuard) SetLocked(ctx context.Context, locked bool) error {
	if err := g.Check(ctx); err != nil {
		return err
	}
	return errors.Join(g.setLocked(ctx, locked, false), g.Check(ctx))
}

// Relock restores the guard on the originally opened filesystems even if their
// mount paths disappeared. Root drift is still reported, but never redirects
// cleanup or prevents an attempt to relock the original root and other mirrors.
func (g *DirectoryGuard) Relock(ctx context.Context) error {
	if len(g.roots) == 0 {
		return os.ErrClosed
	}
	return errors.Join(g.Check(ctx), g.setLocked(ctx, true, true), g.Check(ctx))
}

func (g *DirectoryGuard) setLocked(ctx context.Context, locked, cleanup bool) error {
	mode := os.FileMode(0744)
	if locked {
		mode = 0544
	}
	var failures []error
	for _, root := range g.roots {
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
			return nil
		})
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}
