package rootfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
)

// Set owns the roots for an entire operation. Scans, publication, and directory
// guards borrow these descriptors; only the owner closes them, after cleanup.
type Set struct {
	paths []string
	roots map[string]*Root
}

func OpenSet(ctx context.Context, paths []string) (*Set, error) {
	if len(paths) == 0 {
		return nil, errors.New("mirror roots are required")
	}
	s := &Set{paths: slices.Clone(paths), roots: make(map[string]*Root, len(paths))}
	slices.Sort(s.paths)
	for _, path := range s.paths {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, s.Close())
		}
		if s.roots[path] != nil {
			return nil, errors.Join(fmt.Errorf("duplicate mirror root: %s", path), s.Close())
		}
		root, err := Open(path)
		if err != nil {
			return nil, errors.Join(err, s.Close())
		}
		s.roots[path] = root
		if err := root.CheckInternal(); err != nil {
			return nil, errors.Join(err, s.Close())
		}
	}
	return s, nil
}

func (s *Set) Paths() []string { return slices.Clone(s.paths) }

// Get borrows a root, or returns nil for paths outside this set.
func (s *Set) Get(path string) *Root { return s.roots[path] }

func (s *Set) Check(ctx context.Context) error {
	if len(s.roots) == 0 {
		return os.ErrClosed
	}
	for _, path := range s.paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.roots[path].Check(); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *Set) Close() error {
	var failures []error
	for _, path := range s.paths {
		if root := s.roots[path]; root != nil {
			failures = append(failures, root.Close())
			delete(s.roots, path)
		}
	}
	s.roots = nil
	return errors.Join(failures...)
}
