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
)

// Apply requires prior operator confirmation. It validates every safe repair
// before mutation, never applies unresolved findings, and reports their presence
// even when the safe subset succeeds. The CLI uses apply with durable checkpoints.
func Apply(ctx context.Context, log Log, currentRoots []string, preview bool, writer io.Writer) error {
	return errors.Join(apply(ctx, log, currentRoots, preview, writer, nil), unresolvedError(log))
}

// A checkpoint must durably record starts before any publication for that file.
// A failed checkpoint or operational error stops all subsequent repairs.
func apply(ctx context.Context, log Log, currentRoots []string, preview bool, writer io.Writer, checkpoint func(int, string, error) error) error {
	if writer == nil {
		return errors.New("repair output is required")
	}
	rs, err := prepare(log, currentRoots)
	if err != nil {
		return err
	}
	defer rs.close()
	if err := validateFiles(ctx, log, rs, false); err != nil {
		return err
	}
	record := func(i int, state string, failure error) error {
		if !preview && checkpoint != nil {
			return checkpoint(i, state, failure)
		}
		return nil
	}
	for i, repair := range log.Repairs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if repair.State == RepairCompleted {
			continue // Already verified in desired state; do not repair it again.
		}
		if err := record(i, RepairStarted, nil); err != nil {
			return err
		}
		err := applyRepair(ctx, repair, rs, preview, writer)
		if err == nil && !preview {
			err = validateRepair(ctx, repair, rs, true)
		}
		if err != nil {
			return errors.Join(fmt.Errorf("repair %s: %w", repair.RelPath, err), record(i, RepairFailed, err))
		}
		if err := record(i, RepairCompleted, nil); err != nil {
			return err
		}
	}
	if !preview {
		if err := validateFiles(ctx, log, rs, true); err != nil {
			return fmt.Errorf("validate completed repairs: %w", err)
		}
	}
	return ctx.Err()
}

func applyRepair(ctx context.Context, repair Repair, rs roots, preview bool, writer io.Writer) error {
	// Recheck the complete file's evidence immediately before its first mutation.
	if err := validateRepair(ctx, repair, rs, false); err != nil {
		return err
	}
	var source *rootfs.Root
	for _, copy := range repair.Copies {
		if copy.ActualChecksum == "" {
			continue
		}
		actual, err := checksumFile(ctx, rs[copy.Root], repair.RelPath)
		if err != nil {
			return err
		}
		if actual == repair.Checksum {
			source = rs[copy.Root]
			break
		}
	}
	if source == nil {
		return fmt.Errorf("no verified source remains for %s", repair.RelPath)
	}
	for _, copy := range repair.Copies {
		if copy.ActualChecksum == "" {
			continue // Preserve evidence-only sidecars; never fill missing data.
		}
		root := rs[copy.Root]
		actual, err := checksumFile(ctx, root, repair.RelPath)
		if err != nil {
			return err
		}
		if actual != repair.Checksum {
			if actual != copy.ActualChecksum {
				return fmt.Errorf("destination changed immediately before repair: %s", repair.RelPath)
			}
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
		sidecar, err := readSidecar(ctx, root, sidecarPath)
		if err != nil {
			return err
		}
		if sidecar == nil || sidecar.Value != repair.Checksum {
			if !equalSidecars(sidecar, copy.Sidecar) {
				return fmt.Errorf("sidecar changed immediately before repair: %s", sidecarPath)
			}
			if preview {
				if _, err := fmt.Fprintln(writer, "preview: repaired checksumfile:", filepath.Join(copy.Root, sidecarPath)); err != nil {
					return err
				}
			} else {
				oldHash := ""
				if sidecar != nil {
					oldHash = sidecar.Hash
				}
				backup, err := replace(ctx, root, sidecarPath, oldHash, checksumBytes([]byte(repair.Checksum)), func(f *os.File) error { _, err := io.WriteString(f, repair.Checksum); return err })
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
	}
	return ctx.Err()
}
