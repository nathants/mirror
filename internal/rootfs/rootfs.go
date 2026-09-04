// Package rootfs provides Linux descriptor-relative, no-symlink mirror access.
package rootfs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

const Internal = ".mirror-internal"
const marker = "mirror-internal-v1\n"

type Root struct {
	path string
	file *os.File
}

func Open(path string) (*Root, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, fmt.Errorf("invalid mirror root: %q", path)
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return nil, fmt.Errorf("open root %s without symlinks (Linux openat2 required): %w", path, err)
	}
	return &Root{path: path, file: os.NewFile(uintptr(fd), path)}, nil
}
func (r *Root) Close() error { return r.file.Close() }
func (r *Root) Path() string { return r.path }
func ValidPath(path string) bool {
	return path != "" && !strings.ContainsRune(path, 0) && !filepath.IsAbs(path) && filepath.Clean(path) == path && path != ".." && !strings.HasPrefix(path, "../")
}
func UserPath(path string) bool {
	return ValidPath(path) && path != "." && path != Internal && !strings.HasPrefix(path, Internal+"/")
}

// Check rejects an unmounted, replaced, or redirected root path. The descriptor
// also keeps later relative lookups on the filesystem originally opened.
func (r *Root) Check() error {
	current, err := Open(r.path)
	if err != nil {
		return err
	}
	defer current.Close()
	before, err := r.file.Stat()
	if err != nil {
		return err
	}
	after, err := current.file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) {
		return fmt.Errorf("mirror root changed: %s", r.path)
	}
	return nil
}
func (r *Root) OpenFile(path string, flags int, mode os.FileMode) (*os.File, error) {
	if err := r.Check(); err != nil {
		return nil, err
	}
	return r.openFile(path, flags, mode)
}

// openFile confines access to the retained descriptor, without reopening the
// root path. Only pinned directory-guard cleanup may use it after path loss.
func (r *Root) openFile(path string, flags int, mode os.FileMode) (*os.File, error) {
	if !ValidPath(path) {
		return nil, fmt.Errorf("invalid root-relative path: %q", path)
	}
	fd, err := unix.Openat2(int(r.file.Fd()), path, &unix.OpenHow{Flags: uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW), Mode: uint64(mode.Perm()), Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
	if err != nil {
		return nil, &os.PathError{Op: "open beneath root", Path: filepath.Join(r.path, path), Err: err}
	}
	return os.NewFile(uintptr(fd), filepath.Join(r.path, path)), nil
}
func (r *Root) ReadFile(path string, limit int64) ([]byte, error) {
	if err := r.Check(); err != nil {
		return nil, err
	}
	return r.readFile(path, limit)
}
func (r *Root) readFile(path string, limit int64) ([]byte, error) {
	f, err := r.openFile(path, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", f.Name())
	}
	if limit < 0 || info.Size() > limit {
		return nil, fmt.Errorf("file exceeds metadata limit: %s", f.Name())
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file grew beyond metadata limit: %s", f.Name())
	}
	return data, err
}
func (r *Root) Stat(path string) (os.FileInfo, error) {
	f, err := r.OpenFile(path, unix.O_PATH, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}
func (r *Root) ReadDir(path string) ([]os.DirEntry, error) {
	f, err := r.OpenFile(path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, err
}
func (r *Root) parent(path string) (*os.File, string, error) {
	if !ValidPath(path) || path == "." {
		return nil, "", fmt.Errorf("invalid child path: %q", path)
	}
	f, err := r.OpenFile(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY, 0)
	return f, filepath.Base(path), err
}
func (r *Root) Mkdir(path string, mode os.FileMode) error {
	f, name, err := r.parent(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Mkdirat(int(f.Fd()), name, uint32(mode.Perm())); err != nil {
		return err
	}
	return f.Sync()
}
func (r *Root) MkdirAll(path string, mode os.FileMode) error {
	if !ValidPath(path) {
		return fmt.Errorf("invalid directory: %q", path)
	}
	if path == "." {
		return nil
	}
	current := ""
	for _, part := range strings.Split(path, "/") {
		current = filepath.Join(current, part)
		err := r.Mkdir(current, mode)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		f, err := r.OpenFile(current, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}
func (r *Root) SyncDir(path string) error {
	f, err := r.OpenFile(path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
func (r *Root) Remove(path string) error {
	f, name, err := r.parent(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Unlinkat(int(f.Fd()), name, 0)
}
func (r *Root) Link(old, new string) error {
	a, an, err := r.parent(old)
	if err != nil {
		return err
	}
	defer a.Close()
	b, bn, err := r.parent(new)
	if err != nil {
		return err
	}
	defer b.Close()
	return unix.Linkat(int(a.Fd()), an, int(b.Fd()), bn, 0)
}
func (r *Root) Rename(old, new string, noReplace bool) error {
	a, an, err := r.parent(old)
	if err != nil {
		return err
	}
	defer a.Close()
	b, bn, err := r.parent(new)
	if err != nil {
		return err
	}
	defer b.Close()
	flags := uint(0)
	if noReplace {
		flags = unix.RENAME_NOREPLACE
	}
	return unix.Renameat2(int(a.Fd()), an, int(b.Fd()), bn, flags)
}
func (r *Root) CreateTemp(dir, prefix string) (*os.File, string, error) {
	for {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", err
		}
		path := filepath.Join(dir, prefix+hex.EncodeToString(random[:]))
		f, err := r.OpenFile(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return f, path, err
	}
}

// CheckInternal is observational. Existing unmarked user content is never adopted.
func (r *Root) CheckInternal() error {
	if err := r.Check(); err != nil {
		return err
	}
	return r.checkInternal()
}
func (r *Root) checkInternal() error {
	file, err := r.openFile(Internal, unix.O_PATH, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("reserved directory must be a private real directory: %s", filepath.Join(r.path, Internal))
	}
	data, err := r.readFile(Internal+"/FORMAT", int64(len(marker)))
	if err != nil || string(data) != marker {
		return fmt.Errorf("reserved directory collision at %s; expected mirror FORMAT marker: %w", filepath.Join(r.path, Internal), errors.Join(err, errors.New("invalid marker")))
	}
	return nil
}
func (r *Root) EnsureInternal() error {
	err := r.Mkdir(Internal, 0700)
	if err == nil {
		f, err := r.OpenFile(Internal+"/FORMAT", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := io.WriteString(f, marker)
		err = errors.Join(writeErr, f.Sync(), f.Close())
		if err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := r.CheckInternal(); err != nil {
		return err
	}
	for _, dir := range []string{"tmp", "quarantine"} {
		if err := r.MkdirAll(Internal+"/"+dir, 0700); err != nil {
			return err
		}
	}
	return errors.Join(r.SyncDir(Internal), r.SyncDir("."))
}

func (r *Root) Readlink(path string) (string, error) {
	f, name, err := r.parent(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buffer := make([]byte, 4096)
	n, err := unix.Readlinkat(int(f.Fd()), name, buffer)
	if err != nil {
		return "", err
	}
	if n == len(buffer) {
		return "", errors.New("symlink target too long")
	}
	return string(buffer[:n]), nil
}
func (r *Root) Symlink(target, path string) error {
	f, name, err := r.parent(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Symlinkat(target, int(f.Fd()), name)
}

func (r *Root) MkdirTemp(dir string) (string, error) {
	for {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		path := filepath.Join(dir, hex.EncodeToString(random[:]))
		err := r.Mkdir(path, 0700)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return path, err
	}
}
