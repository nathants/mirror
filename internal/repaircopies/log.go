package repaircopies

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

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

func Load(path string) (Log, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return Log{}, fmt.Errorf("repair log path must be a clean absolute path: %q", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Log{}, fmt.Errorf("inspect repair log: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Log{}, fmt.Errorf("repair log is not a regular file: %s", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Log{}, fmt.Errorf("repair log permissions must not allow group or other access: %o", info.Mode().Perm())
	}
	file, err := os.Open(path)
	if err != nil {
		return Log{}, fmt.Errorf("open repair log: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var log Log
	if err := decoder.Decode(&log); err != nil {
		return Log{}, fmt.Errorf("decode repair log: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Log{}, errors.New("repair log contains trailing JSON data")
		}
		return Log{}, fmt.Errorf("decode trailing repair log data: %w", err)
	}
	if err := validateLog(log); err != nil {
		return Log{}, fmt.Errorf("validate repair log: %w", err)
	}
	return log, nil
}

func marshalLog(log Log) ([]byte, error) {
	data, err := json.MarshalIndent(log, "", "  ")
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
