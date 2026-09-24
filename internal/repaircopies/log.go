package repaircopies

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Save atomically publishes a new scan or a compacted journal snapshot. Per-file
// checkpoints must use Journal.Append rather than rewriting this evidence.
func Save(path string, log Log) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("repair log path must be a clean absolute path: %q", path)
	}
	if err := validateLog(log); err != nil {
		return fmt.Errorf("validate repair log: %w", err)
	}
	data, err := marshalLog(log)
	if err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create mirror state directory: %w", err)
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspect mirror state directory: %w", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("mirror state path is not a real directory: %s", parent)
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		return fmt.Errorf("secure mirror state directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("repair log path is not a regular file: %s", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect repair log path: %w", err)
	}

	temporary, err := os.CreateTemp(parent, ".repair-last-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary repair log: %w", err)
	}
	temporaryPath := temporary.Name()
	keepTemporary := true
	defer func() {
		if keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary repair log: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary repair log: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary repair log: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary repair log: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish repair log: %w", err)
	}
	keepTemporary = false
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("sync mirror state directory: %w", err)
	}
	return nil
}

// Load is observational: it replays complete progress records without changing
// the journal. Only an unterminated final record may be ignored after a crash.
func Load(path string) (Log, error) {
	file, err := openLog(path, os.O_RDONLY)
	if err != nil {
		return Log{}, err
	}
	log, _, err := readLog(file)
	return log, errors.Join(err, file.Close())
}

func openLog(path string, flags int) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("repair log path must be a clean absolute path: %q", path)
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
		Flags: uint64(flags | unix.O_NONBLOCK | unix.O_CLOEXEC), Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return nil, fmt.Errorf("open repair log: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("repair log is not a regular file")
	}
	if err == nil && info.Mode().Perm()&0077 != 0 {
		err = errors.New("repair log permissions must not allow group or other access")
	}
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func readLog(file *os.File) (Log, bool, error) {
	reader := bufio.NewReader(file)
	snapshot, err := reader.ReadBytes('\n')
	if err != nil {
		return Log{}, false, fmt.Errorf("incomplete repair snapshot; run a fresh scan: %w", err)
	}
	var log Log
	if err := decodeRecord(snapshot, &log); err != nil {
		return Log{}, false, fmt.Errorf("decode repair snapshot; old versions require a fresh scan: %w", err)
	}
	if err := validateLog(log); err != nil {
		return Log{}, false, fmt.Errorf("validate repair log: %w", err)
	}
	sequence := 0
	for {
		data, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return log, sequence > 0 || len(data) > 0, nil
		}
		if err != nil {
			return Log{}, false, err
		}
		var record journalRecord
		if err := decodeRecord(data, &record); err != nil {
			return Log{}, false, fmt.Errorf("decode repair progress record: %w", err)
		}
		if record.Sequence != sequence+1 {
			return Log{}, false, errors.New("repair progress record sequence is not contiguous")
		}
		if err := validateProgress(log, record.Progress); err != nil {
			return Log{}, false, err
		}
		applyProgress(&log, record.Progress)
		sequence++
	}
}

func decodeRecord(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("repair journal record contains trailing data")
	}
	return nil
}

func marshalLog(log Log) ([]byte, error) {
	data, err := json.Marshal(log)
	if err != nil {
		return nil, fmt.Errorf("encode repair log: %w", err)
	}
	return append(data, '\n'), nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
