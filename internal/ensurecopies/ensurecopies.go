// Package ensurecopies ingests checksum metadata and fills missing copies.
package ensurecopies

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"mirror/internal/mountdisks"
	"mirror/internal/repaircopies"
	"mirror/internal/rootfs"
	"mirror/lib"
)

type action struct {
	source, destination, path, checksum string
	target                              *string
	metadata                            bool
}

// Options supplies the command's inventory and output boundaries.
// Snapshot must freshly resolve the configured mounted mirrors on every call.
// The caller must hold the operation lock for the entire Run.
type Options struct {
	Snapshot func(context.Context) (mountdisks.Snapshot, error)
	Output   io.Writer
}

func Run(ctx context.Context, preview bool, options Options) (returnErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if options.Snapshot == nil || options.Output == nil {
		return errors.New("ensure snapshot and output are required")
	}
	state, err := options.Snapshot(ctx)
	if err != nil {
		return err
	}
	actions, err := plan(ctx, state.Roots)
	if err != nil {
		return err
	}
	if preview {
		return apply(ctx, actions, true, options.Output)
	}
	if len(actions) == 0 {
		return ctx.Err()
	}
	// Only planned write destinations need directory mutations. A data source
	// is also a destination when it needs a checksum sidecar.
	writes := make(map[string]bool)
	for _, a := range actions {
		writes[a.destination] = true
	}
	var destinations []string
	for _, root := range state.Roots {
		if writes[root] {
			destinations = append(destinations, root)
		}
	}
	guard, err := lib.OpenDirectoryGuard(ctx, state.Roots, destinations)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, guard.Close()) }()
	current, err := options.Snapshot(ctx)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, state) {
		return errors.New("mounted mirror state changed during planning")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		returnErr = errors.Join(returnErr, guard.Relock(cleanup))
	}()
	if err := guard.SetLocked(ctx, false); err != nil {
		return err
	}
	return apply(ctx, actions, false, options.Output)
}
func plan(ctx context.Context, paths []string) ([]action, error) {
	observations, err := repaircopies.Scan(ctx, paths)
	if err != nil {
		return nil, err
	}
	links := make(map[string]map[string]string)
	for _, path := range paths {
		root, err := rootfs.Open(path)
		if err != nil {
			return nil, err
		}
		err = scanLinks(ctx, root, ".", links)
		closeErr := root.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return nil, err
		}
	}
	all := make(map[string]bool)
	for path, copies := range observations {
		// Scan also returns sidecar-only repair evidence. It is not an
		// existing data copy or a source for ensure's publication plan.
		dataCopies := copies[:0]
		for _, copy := range copies {
			if copy.ActualChecksum != "" {
				dataCopies = append(dataCopies, copy)
			}
		}
		if len(dataCopies) == 0 {
			return nil, fmt.Errorf("no data source for %q: .b2sum is reserved for checksum sidecars, not payload files; restore missing data or rename an intended payload before ingest", path)
		}
		observations[path] = dataCopies
		all[path] = true
	}
	for path := range links {
		all[path] = true
	}
	names := make([]string, 0, len(all))
	for path := range all {
		names = append(names, path)
	}
	sort.Strings(names)
	var actions []action
	for _, path := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		copies := observations[path]
		linkCopies := links[path]
		if len(copies) > 0 && len(linkCopies) > 0 {
			return nil, fmt.Errorf("entry type conflict: %s", path)
		}
		present := make(map[string]bool)
		source := ""
		checksum := ""
		var target *string
		if len(copies) > 0 {
			source = copies[0].Root
			checksum = copies[0].ActualChecksum
			for _, copy := range copies {
				present[copy.Root] = true
				if copy.ActualChecksum != checksum || (copy.Sidecar != nil && copy.Sidecar.Value != checksum) {
					return nil, fmt.Errorf("unresolved checksum disagreement for %s; run repair-copies first", path)
				}
				if copy.Sidecar == nil {
					actions = append(actions, action{destination: copy.Root, path: path, checksum: checksum, metadata: true})
				}
			}
		} else {
			for _, root := range paths {
				value, found := linkCopies[root]
				if !found {
					continue
				}
				present[root] = true
				if target == nil {
					v := value
					target = &v
					source = root
				} else if *target != value {
					return nil, fmt.Errorf("symlink disagreement: %s", path)
				}
			}
		}
		for _, destination := range paths {
			if present[destination] {
				continue
			}
			root, err := rootfs.Open(destination)
			if err != nil {
				return nil, err
			}
			if target == nil {
				err = repaircopies.CheckMissingDestination(ctx, root, path)
			} else {
				_, err = root.Stat(path)
				if errors.Is(err, os.ErrNotExist) {
					err = nil
				} else {
					err = fmt.Errorf("unexpected destination entry %s: %w", filepath.Join(destination, path), errors.Join(err, errors.New("not absent")))
				}
			}
			if err := errors.Join(err, root.Close()); err != nil {
				return nil, err
			}
			actions = append(actions, action{source: source, destination: destination, path: path, checksum: checksum, target: target})
		}
	}
	return actions, nil
}
func scanLinks(ctx context.Context, root *rootfs.Root, dir string, links map[string]map[string]string) error {
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
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if err := scanLinks(ctx, root, rel, links); err != nil {
				return err
			}
		} else if entry.Type()&os.ModeSymlink != 0 {
			target, err := root.Readlink(rel)
			if err != nil {
				return err
			}
			// Preserve links as opaque entries, never follow them while copying.
			if links[rel] == nil {
				links[rel] = make(map[string]string)
			}
			links[rel][root.Path()] = target
		}
	}
	return nil
}
func apply(ctx context.Context, actions []action, preview bool, writer io.Writer) error {
	for _, a := range actions {
		if err := ctx.Err(); err != nil {
			return err
		}
		if preview {
			if _, err := fmt.Fprintf(writer, "preview: publish %s on %s (metadata=%v)\n", a.path, a.destination, a.metadata); err != nil {
				return err
			}
			continue
		}
		var err error
		switch {
		case a.metadata:
			err = repaircopies.PublishMissingSidecar(ctx, a.destination, a.path, a.checksum)
		case a.target != nil:
			err = publishLink(ctx, a)
		default:
			err = repaircopies.PublishMissing(ctx, a.source, a.destination, a.path, a.checksum)
		}
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(writer, "published %s on %s (metadata=%v)\n", a.path, a.destination, a.metadata); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func publishLink(ctx context.Context, a action) error {
	src, err := rootfs.Open(a.source)
	if err != nil {
		return err
	}
	defer src.Close()
	target, err := src.Readlink(a.path)
	if err != nil {
		return err
	}
	if target != *a.target {
		return errors.New("source symlink changed")
	}
	dst, err := rootfs.Open(a.destination)
	if err != nil {
		return err
	}
	defer dst.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dst.MkdirAll(filepath.Dir(a.path), 0755); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dst.Symlink(target, a.path); err != nil {
		return err
	}
	return dst.SyncDir(filepath.Dir(a.path))
}
