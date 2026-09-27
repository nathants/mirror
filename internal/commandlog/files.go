package commandlog

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"mirror/internal/rootfs"
)

const directory = ".mirror-logs"
const timestampLayout = "20060102T150405.000000000Z"

func openDirectory(home string, uid, gid int) (*os.File, error) {
	root, err := rootfs.Open(home)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	created := false
	if err := root.Mkdir(directory, 0700); err == nil {
		created = true
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	dir, err := root.OpenFile(directory, unix.O_RDONLY|unix.O_DIRECTORY, 0)
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
	if err == nil && (int(stat.Uid) != uid || stat.Mode&07777 != 0700) {
		err = errors.New("command log directory must be private (0700) and owned by the caller")
	}
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	// All creation/pruning below uses this validated descriptor, not a reopened
	// path that could name a replacement directory during a sudo invocation.
	return dir, nil
}

func openFile(dir *os.File, name string, flags int, mode uint64) (*os.File, error) {
	fd, err := unix.Openat2(int(dir.Fd()), name, &unix.OpenHow{
		Flags: uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW), Mode: mode,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	})
	if err != nil {
		return nil, &os.PathError{Op: "open command log", Path: filepath.Join(dir.Name(), name), Err: err}
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name)), nil
}

func createFile(dir *os.File, uid, gid int, now time.Time) (*os.File, error) {
	for {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		name := "mirror-" + now.UTC().Format(timestampLayout) + "-" + hex.EncodeToString(random[:]) + ".jsonl"
		file, err := openFile(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// Hold a per-file lock until close so another invocation cannot prune an
		// active command, even if it has been silent for more than 14 days.
		if err := errors.Join(file.Chown(uid, gid), unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)); err != nil {
			_ = file.Close()
			return nil, err
		}
		return file, nil
	}
}

func isLogName(name string) bool {
	body, ok := strings.CutPrefix(name, "mirror-")
	if !ok {
		return false
	}
	body, ok = strings.CutSuffix(body, ".jsonl")
	if !ok || len(body) != len(timestampLayout)+1+32 || body[len(timestampLayout)] != '-' {
		return false
	}
	if _, err := time.Parse(timestampLayout, body[:len(timestampLayout)]); err != nil {
		return false
	}
	_, err := hex.DecodeString(body[len(timestampLayout)+1:])
	return err == nil
}

func prune(dir *os.File, uid int, cutoff time.Time) error {
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	var firstErr error
	for _, entry := range entries {
		if !isLogName(entry.Name()) {
			continue
		}
		if err := pruneFile(dir, entry.Name(), uid, cutoff); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = fmt.Errorf("prune %s: %w", entry.Name(), err)
		}
	}
	return firstErr
}

func expired(stat *unix.Stat_t, uid int, cutoff time.Time) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFREG && int(stat.Uid) == uid && stat.Nlink == 1 && time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec).Before(cutoff)
}

func pruneFile(dir *os.File, name string, uid int, cutoff time.Time) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if !expired(&stat, uid, cutoff) {
		return nil
	}
	file, err := openFile(dir, name, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil
		}
		return err
	}
	// Recheck after acquiring the lock: another invocation may have finished,
	// written more output, or removed this directory entry since enumeration.
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if !expired(&stat, uid, cutoff) {
		return nil
	}
	var current unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if current.Dev != stat.Dev || current.Ino != stat.Ino {
		return nil
	}
	return unix.Unlinkat(int(dir.Fd()), name, 0)
}
