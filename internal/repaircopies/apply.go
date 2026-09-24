package repaircopies

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mirror/internal/cancelio"
	"mirror/internal/rootfs"
	"mirror/lib"
)

// Apply requires prior operator confirmation. It validates every safe repair
// before mutation, never applies unresolved findings, and reports their presence
// even when the safe subset succeeds. The CLI adds inventory revalidation and
// durable checkpoints to the same disk-at-a-time application.
func Apply(ctx context.Context, log Log, rs *rootfs.Set, preview bool, writer io.Writer) error {
	return errors.Join(apply(ctx, log, rs, preview, writer, nil, nil), unresolvedError(log))
}

// The caller confirms before application. Full-plan validation selects only
// still-needed writes, including on resume; each disk's file evidence is checked
// again immediately before publication. Starts precede the first write on any
// disk, and completion requires desired bytes on every copy. Thus a file can
// remain started across disk batches without changing the immutable scan.
func apply(ctx context.Context, log Log, rs *rootfs.Set, preview bool, writer io.Writer, checkpoint func(int, string, error) error, revalidate func(context.Context) error) error {
	if writer == nil {
		return errors.New("repair output is required")
	}
	if err := prepare(ctx, log, rs); err != nil {
		return err
	}
	if !preview {
		if err := lib.CheckWriteInputs(ctx, rs); err != nil {
			return err
		}
	}
	initial, err := validateFiles(ctx, log, rs, false)
	if err != nil {
		return err
	}
	if preview {
		for i, repair := range log.Repairs {
			if repair.State == RepairCompleted {
				continue
			}
			for _, copy := range initial[i].copies {
				if err := applyCopy(ctx, repair, rs, initial[i].source, copy, true, writer); err != nil {
					return err
				}
			}
		}
		return ctx.Err()
	}

	batches := make(map[string][]int)
	remaining := make([]int, len(log.Repairs))
	for i, repair := range log.Repairs {
		if repair.State == RepairCompleted {
			continue
		}
		for _, copy := range initial[i].copies {
			if copy.ActualChecksum != repair.Checksum || copy.Sidecar == nil || copy.Sidecar.Value != repair.Checksum {
				batches[copy.Root] = append(batches[copy.Root], i)
				remaining[i]++
			}
		}
	}
	record := func(i int, state string, failure error) error {
		if checkpoint != nil {
			return checkpoint(i, state, failure)
		}
		return nil
	}
	started := make([]bool, len(log.Repairs))
	start := func(i int) error {
		if !started[i] {
			if err := record(i, RepairStarted, nil); err != nil {
				return err
			}
			started[i] = true
		}
		return nil
	}
	failed := func(i int, err error) error {
		return errors.Join(fmt.Errorf("repair %s: %w", log.Repairs[i].RelPath, err), record(i, RepairFailed, err))
	}
	complete := func(i int) error {
		if _, err := validateRepair(ctx, log.Repairs[i], rs, true); err != nil {
			return failed(i, err)
		}
		return record(i, RepairCompleted, nil)
	}
	// A prior run may have published everything but lost its completion record.
	// Revalidate and checkpoint such files without opening any write window.
	for i, repair := range log.Repairs {
		if repair.State != RepairCompleted && remaining[i] == 0 {
			if err := start(i); err != nil {
				return err
			}
			if err := complete(i); err != nil {
				return err
			}
		}
	}
	for _, path := range rs.Paths() {
		batch := batches[path]
		if len(batch) == 0 {
			continue
		}
		if revalidate != nil {
			if err := revalidate(ctx); err != nil {
				return err
			}
		}
		if err := lib.WithWriteDisk(ctx, rs, path, func() error {
			if revalidate != nil {
				if err := revalidate(ctx); err != nil {
					return err
				}
			}
			for _, i := range batch {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := start(i); err != nil {
					return err
				}
				repair := log.Repairs[i]
				verified, err := validateRepair(ctx, repair, rs, false)
				if err != nil {
					return failed(i, err)
				}
				for _, copy := range verified.copies {
					if copy.Root == path {
						if err := applyCopy(ctx, repair, rs, verified.source, copy, false, writer); err != nil {
							return failed(i, err)
						}
					}
				}
				remaining[i]--
				if remaining[i] == 0 {
					if err := complete(i); err != nil {
						return err
					}
				}
			}
			return ctx.Err()
		}); err != nil {
			return err
		}
	}
	if _, err := validateFiles(ctx, log, rs, true); err != nil {
		return fmt.Errorf("validate completed repairs: %w", err)
	}
	return ctx.Err()
}

func applyCopy(ctx context.Context, repair Repair, rs *rootfs.Set, source *rootfs.Root, copy Copy, preview bool, writer io.Writer) error {
	root := rs.Get(copy.Root)
	actual := copy.ActualChecksum
	if actual != repair.Checksum {
		if preview {
			if _, err := fmt.Fprintln(writer, "preview: repaired file:", filepath.Join(copy.Root, repair.RelPath)); err != nil {
				return err
			}
		} else {
			backup, err := replace(ctx, root, repair.RelPath, actual, repair.Checksum, func(f *os.File) error {
				src, err := openRegular(source, repair.RelPath)
				if err != nil {
					return err
				}
				_, copyErr := cancelio.Copy(ctx, f, src)
				return errors.Join(copyErr, src.Close())
			})
			if err != nil {
				return fmt.Errorf("repair file %s: %w", repair.RelPath, err)
			}
			if _, err := fmt.Fprintln(writer, "preserved corrupt file:", backup); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(writer, "repaired file:", filepath.Join(copy.Root, repair.RelPath)); err != nil {
				return err
			}
		}
	}
	sidecarPath := repair.RelPath + checksumSuffix
	sidecar := copy.Sidecar
	if sidecar == nil || sidecar.Value != repair.Checksum {
		if preview {
			if _, err := fmt.Fprintln(writer, "preview: repaired checksumfile:", filepath.Join(copy.Root, sidecarPath)); err != nil {
				return err
			}
		} else {
			oldHash := ""
			if sidecar != nil {
				oldHash = sidecar.Hash
			}
			backup, err := publishSidecar(ctx, root, repair.RelPath, repair.Checksum, oldHash)
			if err != nil {
				return fmt.Errorf("repair sidecar %s: %w", sidecarPath, err)
			}
			if backup != "" {
				if _, err := fmt.Fprintln(writer, "preserved corrupt checksumfile:", backup); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintln(writer, "repaired checksumfile:", filepath.Join(copy.Root, sidecarPath)); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}
