package lib

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
			return nil
		})
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}
