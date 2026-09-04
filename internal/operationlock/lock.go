// Package operationlock serializes all mirror operations for one caller home.
package operationlock

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
	"mirror/internal/rootfs"
)

const lockPath = ".mirror/operation.lock"

// Acquire never waits, unlinks, or replaces the lock inode. Kernel flock is
// released on close/process exit, including SIGKILL; stale PID text is harmless.
func Acquire(ctx context.Context, home string, uid, gid int) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := rootfs.Open(home)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	created := false
	if err := root.Mkdir(".mirror", 0700); err == nil {
		created = true
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	dir, err := root.OpenFile(".mirror", unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	if created {
		err = dir.Chown(uid, gid)
	}
	var stat unix.Stat_t
	if err == nil {
		err = unix.Fstat(int(dir.Fd()), &stat)
	}
	closeErr := dir.Close()
	if err := errors.Join(err, closeErr); err != nil {
		return nil, err
	}
	if int(stat.Uid) != uid || stat.Mode&0022 != 0 {
		return nil, errors.New("mirror state directory has unsafe ownership or writable group/other permissions")
	}
	file, err := root.OpenFile(lockPath, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0600)
	if err == nil {
		if err := file.Chown(uid, gid); err != nil {
			file.Close()
			return nil, err
		}
	} else if errors.Is(err, os.ErrExist) {
		file, err = root.OpenFile(lockPath, unix.O_RDWR|unix.O_NONBLOCK, 0)
	}
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*os.File, error) { file.Close(); return nil, err }
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fail(err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || int(stat.Uid) != uid || stat.Mode&0777 != 0600 {
		return fail(errors.New("operation lock must be a private, single-link regular file owned by the caller"))
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return fail(errors.New("another mirror operation is running"))
		}
		return fail(err)
	}
	info, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	current, err := root.Stat(lockPath)
	if err != nil {
		return fail(err)
	}
	if !os.SameFile(info, current) {
		return fail(errors.New("operation lock path changed during acquisition"))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := file.Truncate(0); err != nil {
		return fail(err)
	}
	if _, err := fmt.Fprintf(file, "pid=%d\n", os.Getpid()); err != nil {
		return fail(err)
	}
	return file, nil
}
