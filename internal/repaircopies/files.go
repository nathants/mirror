package repaircopies

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/blake2b"
	"golang.org/x/sys/unix"
	"mirror/internal/cancelio"
	"mirror/internal/rootfs"
)

func openRegular(root *rootfs.Root, path string) (*os.File, error) {
	f, err := root.OpenFile(path, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("not a regular file: %s", f.Name())
	}
	return f, nil
}
func checksumFile(ctx context.Context, root *rootfs.Root, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f, err := openRegular(root, path)
	if err != nil {
		return "", err
	}
	h, _ := blake2b.New512(nil)
	_, copyErr := cancelio.Copy(ctx, h, f)
	err = errors.Join(copyErr, f.Close())
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// prefixCapture bounds metadata memory even when a damaged sidecar is huge.
type prefixCapture struct{ data []byte }

func (p *prefixCapture) Write(data []byte) (int, error) {
	n := len(data)
	remaining := 129 - len(p.data)
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		p.data = append(p.data, data...)
	}
	return n, nil
}
func readSidecar(ctx context.Context, root *rootfs.Root, path string) (*Sidecar, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := openRegular(root, path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	h, _ := blake2b.New512(nil)
	prefix := &prefixCapture{}
	_, copyErr := cancelio.Copy(ctx, io.MultiWriter(h, prefix), f)
	err = errors.Join(copyErr, f.Close())
	if err != nil {
		return nil, err
	}
	result := &Sidecar{Hash: hex.EncodeToString(h.Sum(nil))}
	if value := string(prefix.data); validChecksum(value) {
		result.Value = value
	}
	return result, nil
}

// Scan is read-only, including when metadata is missing or malformed. It does
// not generate a checksum sidecar, chmod data, or admit quarantine/temporary files.
func Scan(ctx context.Context, rs *rootfs.Set) (map[string][]Copy, error) {
	return scan(ctx, rs, nil)
}

// Keep one sequential reader per disk, while scanning independent disks in
// parallel. On any error all readers are cancelled and joined before returning.
func scan(ctx context.Context, rs *rootfs.Set, progress func(string, string)) (map[string][]Copy, error) {
	paths := rs.Paths()
	if err := validateCanonicalRoots(paths); err != nil {
		return nil, err
	}
	if err := rs.Check(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([]map[string]Copy, len(paths))
	failures := make([]error, len(paths))
	var workers sync.WaitGroup
	var progressLock sync.Mutex
	report := func(root, path string) {
		if progress != nil {
			progressLock.Lock()
			defer progressLock.Unlock()
			progress(root, path)
		}
	}
	for i, path := range paths {
		workers.Add(1)
		go func(i int, root *rootfs.Root) {
			defer workers.Done()
			results[i], failures[i] = scanRoot(ctx, root, report)
			if failures[i] != nil {
				cancel()
			}
		}(i, rs.Get(path))
	}
	workers.Wait()
	if err := errors.Join(failures...); err != nil {
		return nil, err
	}
	observations := make(map[string][]Copy)
	for _, result := range results {
		for path, copy := range result {
			observations[path] = append(observations[path], copy)
		}
	}
	return observations, rs.Check(ctx)
}
func scanRoot(ctx context.Context, root *rootfs.Root, progress func(string, string)) (map[string]Copy, error) {
	observations := make(map[string]Copy)
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
			if err := ctx.Err(); err != nil {
				return err
			}
			rel := filepath.Join(dir, entry.Name())
			if rel == rootfs.Internal || rel == "lost+found" {
				continue
			}
			if err := validateRelativePath(rel); err != nil {
				return err
			}
			switch {
			case entry.IsDir():
				if err := walk(rel); err != nil {
					return err
				}
			case entry.Type()&os.ModeSymlink != 0:
				if err := validateDataPath(rel); err != nil {
					return err
				}
				continue
			case entry.Type().IsRegular():
				if strings.HasSuffix(rel, checksumSuffix) {
					dataPath := strings.TrimSuffix(rel, checksumSuffix)
					if err := validateDataPath(dataPath); err != nil {
						return fmt.Errorf("invalid sidecar %q: .b2sum is reserved for checksum sidecars of regular payload files: %w", filepath.Join(root.Path(), rel), err)
					}
					if info, err := root.Stat(dataPath); err == nil {
						if !info.Mode().IsRegular() {
							return fmt.Errorf("invalid sidecar %q: .b2sum is reserved for checksum sidecars of regular payload files", filepath.Join(root.Path(), rel))
						}
						continue // Existing data is observed with its sidecar below.
					} else if !errors.Is(err, os.ErrNotExist) {
						return err
					}
					sidecar, err := readSidecar(ctx, root, rel)
					if err != nil {
						return err
					}
					if sidecar == nil {
						return fmt.Errorf("sidecar disappeared during scan: %s", rel)
					}
					observations[dataPath] = Copy{Root: root.Path(), Sidecar: sidecar}
					if progress != nil {
						progress(root.Path(), rel)
					}
					continue
				}
				actual, err := checksumFile(ctx, root, rel)
				if err != nil {
					return err
				}
				sidecar, err := readSidecar(ctx, root, rel+checksumSuffix)
				if err != nil {
					return err
				}
				observations[rel] = Copy{Root: root.Path(), ActualChecksum: actual, Sidecar: sidecar}
				if progress != nil {
					progress(root.Path(), rel)
				}
			default:
				return fmt.Errorf("unsupported entry: %s", filepath.Join(root.Path(), rel))
			}
		}
		return nil
	}
	if err := walk("."); err != nil {
		return nil, err
	}
	return observations, ctx.Err()
}

func ValidateCurrentState(ctx context.Context, log Log, rs *rootfs.Set) error {
	if err := prepare(ctx, log, rs); err != nil {
		return err
	}
	return validateFiles(ctx, log, rs, false)
}
func prepare(ctx context.Context, log Log, rs *rootfs.Set) error {
	if err := validateLog(log); err != nil {
		return err
	}
	if log.Status != StatusUnresolved {
		if err := RequirePending(log); err != nil {
			return err
		}
	}
	if !equalStrings(log.Roots, rs.Paths()) {
		return fmt.Errorf("mounted roots changed since the logged scan: logged %v, current %v", log.Roots, rs.Paths())
	}
	return rs.Check(ctx)
}
func validateFiles(ctx context.Context, log Log, rs *rootfs.Set, complete bool) error {
	for _, repair := range log.Repairs {
		if _, err := validateRepair(ctx, repair, rs, complete || repair.State == RepairCompleted); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// verifiedRepair is short-lived evidence for one immediate application. Source
// bytes are still checked after staging, destinations before replacement, and
// every copy again before recording completion.
type verifiedRepair struct {
	source *rootfs.Root
	copies []Copy
}

func validateRepair(ctx context.Context, repair Repair, rs *rootfs.Set, complete bool) (verifiedRepair, error) {
	verified := verifiedRepair{}
	observed := make(map[string]bool, len(repair.Copies))
	for _, copy := range repair.Copies {
		observed[copy.Root] = true
	}
	// An omitted mounted mirror supplied no evidence for this path. New data
	// or metadata could change consensus; neither may appear during resume,
	// application, or completion without a fresh scan.
	for _, path := range rs.Paths() {
		root := rs.Get(path)
		if observed[path] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return verified, err
		}
		if err := root.Check(); err != nil {
			return verified, err
		}
		if err := CheckMissingDestination(ctx, root, repair.RelPath); err != nil {
			return verified, fmt.Errorf("unobserved repair paths must remain absent; inspect the evidence and run a fresh scan: %w", err)
		}
	}
	for _, copy := range repair.Copies {
		root := rs.Get(copy.Root)
		if copy.ActualChecksum == "" {
			if err := validateEvidenceOnly(ctx, root, repair.RelPath, copy.Sidecar); err != nil {
				return verified, err
			}
			continue
		}
		actual, err := checksumFile(ctx, root, repair.RelPath)
		if err != nil {
			return verified, err
		}
		if actual != repair.Checksum && (complete || actual != copy.ActualChecksum) {
			return verified, fmt.Errorf("file changed since the logged scan or is not repaired: %s", filepath.Join(copy.Root, repair.RelPath))
		}
		sidecar, err := readSidecar(ctx, root, repair.RelPath+checksumSuffix)
		if err != nil {
			return verified, err
		}
		desired := sidecar != nil && sidecar.Value == repair.Checksum
		if !desired && (complete || !equalSidecars(sidecar, copy.Sidecar)) {
			return verified, fmt.Errorf("checksum sidecar changed since the logged scan or is not repaired: %s", filepath.Join(copy.Root, repair.RelPath+checksumSuffix))
		}
		verified.copies = append(verified.copies, Copy{Root: copy.Root, ActualChecksum: actual, Sidecar: sidecar})
		if actual == repair.Checksum && verified.source == nil {
			verified.source = root
		}
	}
	if verified.source == nil {
		return verified, fmt.Errorf("no verified source remains for %s", repair.RelPath)
	}
	return verified, ctx.Err()
}

// Evidence-only entries are not repair targets. In both resume and completion,
// data must still be absent and the exact sidecar bytes must remain unchanged.
func validateEvidenceOnly(ctx context.Context, root *rootfs.Root, path string, expected *Sidecar) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := root.Stat(path); err == nil {
		return fmt.Errorf("data appeared at evidence-only path: %s", filepath.Join(root.Path(), path))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sidecar, err := readSidecar(ctx, root, path+checksumSuffix)
	if err != nil {
		return err
	}
	if !equalSidecars(sidecar, expected) {
		return fmt.Errorf("sidecar-only evidence changed since the logged scan: %s", filepath.Join(root.Path(), path+checksumSuffix))
	}
	return ctx.Err()
}

func replace(ctx context.Context, root *rootfs.Root, destination, oldHash, expected string, populate func(*os.File) error) (backup string, returnErr error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := root.EnsureInternal(); err != nil {
		return "", err
	}
	temporary, tempPath, err := root.CreateTemp(rootfs.Internal+"/tmp", "repair-")
	if err != nil {
		return "", err
	}
	defer func() {
		if err := root.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	populateErr := populate(temporary)
	err = errors.Join(populateErr, temporary.Chmod(0444), temporary.Sync(), temporary.Close())
	if err != nil {
		return "", err
	}
	actual, err := checksumFile(ctx, root, tempPath)
	if err != nil {
		return "", err
	}
	if actual != expected {
		return "", fmt.Errorf("temporary replacement checksum is %s, expected %s", actual, expected)
	}
	current, err := checksumFile(ctx, root, destination)
	if oldHash == "" {
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("expected absent destination %s: %w", destination, errors.Join(err, errors.New("destination changed")))
		}
	} else {
		if err != nil {
			return "", err
		}
		if current != oldHash {
			return "", fmt.Errorf("destination changed while preparing replacement: %s", destination)
		}
		record, err := root.MkdirTemp(rootfs.Internal + "/quarantine")
		if err != nil {
			return "", err
		}
		backup = filepath.Join(record, destination)
		if err := root.MkdirAll(filepath.Dir(backup), 0700); err != nil {
			return "", err
		}
		if err := root.Link(destination, backup); err != nil {
			return "", err
		}
		saved, err := checksumFile(ctx, root, backup)
		if err != nil {
			return "", err
		}
		if saved != oldHash {
			return "", fmt.Errorf("destination changed during quarantine: %s", destination)
		}
		if err := root.SyncDir(filepath.Dir(backup)); err != nil {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := root.Rename(tempPath, destination, oldHash == ""); err != nil {
		return "", err
	}
	if err := errors.Join(root.SyncDir(filepath.Dir(destination)), root.SyncDir(rootfs.Internal+"/tmp")); err != nil {
		return "", err
	}
	if backup != "" {
		backup = filepath.Join(root.Path(), backup)
	}
	return backup, nil
}

// CheckMissingDestination checks the complete regular-file publication pair.
// Planning and publication share this read-only check; even matching sidecars
// obstruct publication and must be handled manually, never adopted or replaced.
func CheckMissingDestination(ctx context.Context, root *rootfs.Root, path string) error {
	if err := validateDataPath(path); err != nil {
		return err
	}
	for _, rel := range []string{path, path + checksumSuffix} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := root.Stat(rel); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("destination must be absent: %s: %w", filepath.Join(root.Path(), rel), errors.Join(err, errors.New("not absent")))
		}
	}
	return nil
}

// PublishMissing is the ensure-copies publication path: no existing data or
// metadata may be replaced, and the copied bytes must match the planned hash.
func PublishMissing(ctx context.Context, src, dst *rootfs.Root, path, checksum string) error {
	if err := validateDataPath(path); err != nil {
		return err
	}
	if err := src.Check(); err != nil {
		return err
	}
	if err := CheckMissingDestination(ctx, dst, path); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dst.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	_, err := replace(ctx, dst, path, "", checksum, func(file *os.File) error {
		input, err := openRegular(src, path)
		if err != nil {
			return err
		}
		_, copyErr := cancelio.Copy(ctx, file, input)
		return errors.Join(copyErr, input.Close())
	})
	if err != nil {
		return err
	}
	return PublishMissingSidecar(ctx, dst, path, checksum)
}
func PublishMissingSidecar(ctx context.Context, root *rootfs.Root, path, checksum string) error {
	if err := validateDataPath(path); err != nil {
		return err
	}
	if !validChecksum(checksum) {
		return errors.New("invalid checksum")
	}
	_, err := publishSidecar(ctx, root, path, checksum, "")
	return err
}

// A file-wide validation can precede other publications. Recheck this payload
// at the metadata boundary instead of writing a checksum from stale evidence.
func publishSidecar(ctx context.Context, root *rootfs.Root, path, checksum, oldHash string) (string, error) {
	actual, err := checksumFile(ctx, root, path)
	if err != nil {
		return "", err
	}
	if actual != checksum {
		return "", fmt.Errorf("file changed before metadata publication: %s", path)
	}
	return replace(ctx, root, path+checksumSuffix, oldHash, checksumBytes([]byte(checksum)), func(file *os.File) error { _, err := io.WriteString(file, checksum); return err })
}
