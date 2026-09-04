// Package repaircopies builds, persists, validates, and applies mirror repair plans.
package repaircopies

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/blake2b"
)

const (
	LogVersion = 1

	StatusScanning  = "scanning"
	StatusPending   = "pending"
	StatusClean     = "clean"
	StatusCompleted = "completed"

	checksumSuffix = ".b2sum"
)

type Copy struct {
	Root             string `json:"root"`
	ActualChecksum   string `json:"actual_checksum"`
	RecordedChecksum string `json:"recorded_checksum"`
}

type Repair struct {
	RelPath  string `json:"path"`
	Checksum string `json:"repair_checksum"`
	Copies   []Copy `json:"copies"`
}

type Log struct {
	Version     int       `json:"version"`
	Status      string    `json:"status"`
	StartedAt   time.Time `json:"started_at"`
	ScannedAt   time.Time `json:"scanned_at"`
	CompletedAt time.Time `json:"completed_at"`
	Roots       []string  `json:"roots"`
	Repairs     []Repair  `json:"repairs"`
}

func NewScanningLog(startedAt time.Time, roots []string) (Log, error) {
	log := Log{
		Version:   LogVersion,
		Status:    StatusScanning,
		StartedAt: startedAt.UTC(),
		Roots:     canonicalStrings(roots),
	}
	if err := validateLog(log); err != nil {
		return Log{}, err
	}
	return log, nil
}

// BuildLog turns a completed scan into a canonical repair log. Each data
// checksum and its recorded sidecar checksum gets one vote. A repair is
// accepted only when one checksum has a strict majority and at least one data
// copy actually has that checksum.
func BuildLog(
	startedAt time.Time,
	scannedAt time.Time,
	roots []string,
	observations map[string][]Copy,
) (Log, error) {
	log := Log{
		Version:   LogVersion,
		Status:    StatusClean,
		StartedAt: startedAt.UTC(),
		ScannedAt: scannedAt.UTC(),
		Roots:     canonicalStrings(roots),
	}
	paths := make([]string, 0, len(observations))
	for path := range observations {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		copies := append([]Copy(nil), observations[path]...)
		sort.Slice(copies, func(i, j int) bool { return copies[i].Root < copies[j].Root })
		counts := make(map[string]int)
		for _, copy := range copies {
			if !validChecksum(copy.ActualChecksum) {
				return Log{}, fmt.Errorf("invalid actual checksum for %s on %s: %q", path, copy.Root, copy.ActualChecksum)
			}
			if !validChecksum(copy.RecordedChecksum) {
				return Log{}, fmt.Errorf("invalid recorded checksum for %s on %s: %q", path, copy.Root, copy.RecordedChecksum)
			}
			counts[copy.ActualChecksum]++
			counts[copy.RecordedChecksum]++
		}
		if len(counts) <= 1 {
			continue
		}
		winner, err := uniqueWinner(counts)
		if err != nil {
			return Log{}, fmt.Errorf("cannot safely repair %s: %w", path, err)
		}
		hasSource := false
		for _, copy := range copies {
			if checksumsEqual(copy.ActualChecksum, winner) {
				hasSource = true
				break
			}
		}
		if !hasSource {
			return Log{}, fmt.Errorf("cannot safely repair %s: no copy whose contents match winning checksum %s", path, winner)
		}
		log.Repairs = append(log.Repairs, Repair{RelPath: path, Checksum: winner, Copies: copies})
	}
	if len(log.Repairs) > 0 {
		log.Status = StatusPending
	}
	if err := validateLog(log); err != nil {
		return Log{}, err
	}
	return log, nil
}

func uniqueWinner(counts map[string]int) (string, error) {
	winner := ""
	winnerCount := -1
	total := 0
	tied := false
	for checksum, count := range counts {
		total += count
		switch {
		case count > winnerCount:
			winner = checksum
			winnerCount = count
			tied = false
		case count == winnerCount:
			tied = true
		}
	}
	if winner == "" || tied || winnerCount*2 <= total {
		return "", errors.New("there is no unique strict checksum majority")
	}
	return winner, nil
}

func PrintRepairs(writer io.Writer, log Log) error {
	if writer == nil {
		return errors.New("repair output is required")
	}
	if err := validateLog(log); err != nil {
		return err
	}
	for _, repair := range log.Repairs {
		fmt.Fprintln(writer, repair.RelPath)
		for _, copy := range repair.Copies {
			fmt.Fprintln(writer, "  disk:", copy.Root)
			fmt.Fprintln(writer, "    file:  ", copy.RecordedChecksum)
			fmt.Fprintln(writer, "    actual:", copy.ActualChecksum)
		}
		fmt.Fprintln(writer)
	}
	return nil
}

func MarkCompleted(log Log, completedAt time.Time) (Log, error) {
	if err := RequirePending(log); err != nil {
		return Log{}, err
	}
	log.Status = StatusCompleted
	log.CompletedAt = completedAt.UTC()
	if err := validateLog(log); err != nil {
		return Log{}, err
	}
	return log, nil
}

func RequirePending(log Log) error {
	if log.Status != StatusPending {
		return fmt.Errorf("last repair scan is %s, not pending", log.Status)
	}
	return nil
}

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

func validateLog(log Log) error {
	if log.Version != LogVersion {
		return fmt.Errorf("unsupported repair log version: %d", log.Version)
	}
	if log.StartedAt.IsZero() {
		return errors.New("repair log has no start time")
	}
	if log.StartedAt.Location() != time.UTC {
		return errors.New("repair log start time is not UTC")
	}
	if err := validateCanonicalRoots(log.Roots); err != nil {
		return err
	}
	switch log.Status {
	case StatusScanning:
		if !log.ScannedAt.IsZero() || !log.CompletedAt.IsZero() || len(log.Repairs) != 0 {
			return errors.New("scanning repair log contains completed scan state")
		}
	case StatusClean:
		if log.ScannedAt.IsZero() || !log.CompletedAt.IsZero() || len(log.Repairs) != 0 {
			return errors.New("clean repair log has invalid scan state")
		}
	case StatusPending:
		if log.ScannedAt.IsZero() || !log.CompletedAt.IsZero() || len(log.Repairs) == 0 {
			return errors.New("pending repair log has invalid scan state")
		}
	case StatusCompleted:
		if log.ScannedAt.IsZero() || log.CompletedAt.IsZero() || len(log.Repairs) == 0 {
			return errors.New("completed repair log has invalid completion state")
		}
	default:
		return fmt.Errorf("unknown repair log status: %q", log.Status)
	}
	if !log.ScannedAt.IsZero() {
		if log.ScannedAt.Location() != time.UTC || log.ScannedAt.Before(log.StartedAt) {
			return errors.New("repair log scan time is invalid")
		}
	}
	if !log.CompletedAt.IsZero() {
		if log.CompletedAt.Location() != time.UTC || log.CompletedAt.Before(log.ScannedAt) {
			return errors.New("repair log completion time is invalid")
		}
	}
	previousPath := ""
	for index, repair := range log.Repairs {
		if err := validateRelativePath(repair.RelPath); err != nil {
			return err
		}
		if index > 0 && repair.RelPath <= previousPath {
			return errors.New("repair paths are not sorted and unique")
		}
		previousPath = repair.RelPath
		if !validChecksum(repair.Checksum) {
			return fmt.Errorf("invalid repair checksum for %s: %q", repair.RelPath, repair.Checksum)
		}
		if len(repair.Copies) == 0 {
			return fmt.Errorf("repair has no copies: %s", repair.RelPath)
		}
		previousRoot := ""
		counts := make(map[string]int)
		hasSource := false
		for copyIndex, copy := range repair.Copies {
			if copyIndex > 0 && copy.Root <= previousRoot {
				return fmt.Errorf("copy roots for %s are not sorted and unique", repair.RelPath)
			}
			previousRoot = copy.Root
			if !containsString(log.Roots, copy.Root) {
				return fmt.Errorf("copy root is not in logged roots for %s: %s", repair.RelPath, copy.Root)
			}
			if !validChecksum(copy.ActualChecksum) || !validChecksum(copy.RecordedChecksum) {
				return fmt.Errorf("copy has an invalid checksum for %s on %s", repair.RelPath, copy.Root)
			}
			counts[copy.ActualChecksum]++
			counts[copy.RecordedChecksum]++
			if copy.ActualChecksum == repair.Checksum {
				hasSource = true
			}
		}
		winner, err := uniqueWinner(counts)
		if err != nil || winner != repair.Checksum {
			return fmt.Errorf("repair checksum is not the logged strict majority for %s", repair.RelPath)
		}
		if !hasSource {
			return fmt.Errorf("repair has no logged source copy for %s", repair.RelPath)
		}
	}
	return nil
}

func validateCanonicalRoots(roots []string) error {
	if len(roots) == 0 {
		return errors.New("repair log contains no mirror roots")
	}
	previous := ""
	for index, root := range roots {
		if !utf8.ValidString(root) || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
			return fmt.Errorf("mirror root must be valid UTF-8 and a clean non-root absolute path: %q", root)
		}
		if index > 0 && root <= previous {
			return errors.New("mirror roots are not sorted and unique")
		}
		previous = root
	}
	return nil
}

func validateRelativePath(path string) error {
	if !utf8.ValidString(path) || path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || path == "." || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid relative path in repair log: %q", path)
	}
	return nil
}

func validChecksum(checksum string) bool {
	if len(checksum) != blake2b.Size*2 || strings.ToLower(checksum) != checksum {
		return false
	}
	_, err := hex.DecodeString(checksum)
	return err == nil
}

func checksumsEqual(left, right string) bool {
	return left == right
}

func canonicalStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func containsString(values []string, wanted string) bool {
	index := sort.SearchStrings(values, wanted)
	return index < len(values) && values[index] == wanted
}

// ValidateCurrentState checks that the same mirror roots are mounted and every
// logged file and sidecar is either still in its exact scanned state or has
// already reached the desired state during an interrupted earlier apply.
func ValidateCurrentState(log Log, currentRoots []string) error {
	if err := validateLog(log); err != nil {
		return err
	}
	if err := RequirePending(log); err != nil {
		return err
	}
	currentRoots = canonicalStrings(currentRoots)
	if !equalStrings(log.Roots, currentRoots) {
		return fmt.Errorf("mounted roots changed since the logged scan: logged %v, current %v", log.Roots, currentRoots)
	}
	for _, repair := range log.Repairs {
		hasSource := false
		for _, copy := range repair.Copies {
			path, err := pathBelow(copy.Root, repair.RelPath)
			if err != nil {
				return err
			}
			actual, err := checksumFile(path)
			if err != nil {
				return fmt.Errorf("checksum %s: %w", path, err)
			}
			if !checksumsEqual(actual, copy.ActualChecksum) && !checksumsEqual(actual, repair.Checksum) {
				return fmt.Errorf("file changed since the logged scan: %s has %s", path, actual)
			}
			recorded, err := readRecordedChecksum(path + checksumSuffix)
			if err != nil {
				return err
			}
			if !checksumsEqual(recorded, copy.RecordedChecksum) && !checksumsEqual(recorded, repair.Checksum) {
				return fmt.Errorf("checksum sidecar changed since the logged scan: %s", path+checksumSuffix)
			}
			if checksumsEqual(actual, repair.Checksum) {
				hasSource = true
			}
		}
		if !hasSource {
			return fmt.Errorf("no verified source remains for %s", repair.RelPath)
		}
	}
	return nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func pathBelow(root, relative string) (string, error) {
	if err := validateRelativePath(relative); err != nil {
		return "", err
	}
	path := filepath.Join(root, relative)
	back, err := filepath.Rel(root, path)
	if err != nil || back != relative {
		return "", fmt.Errorf("repair path escapes mirror root: %s below %s", relative, root)
	}
	return path, nil
}

func checksumFile(path string) (checksum string, returnErr error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("not a regular file: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := file.Close(); returnErr == nil && err != nil {
			returnErr = err
		}
	}()
	hasher, err := blake2b.New512(nil)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func checksumBytes(data []byte) string {
	sum := blake2b.Sum512(data)
	return hex.EncodeToString(sum[:])
}

func readRecordedChecksum(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect checksum sidecar %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("checksum sidecar is not a regular file: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read checksum sidecar %s: %w", path, err)
	}
	checksum := string(data)
	if !validChecksum(checksum) {
		return "", fmt.Errorf("checksum sidecar has invalid contents at %s: %q", path, checksum)
	}
	return checksum, nil
}

// Apply validates a pending log and current mirror state before any mutation,
// then rechecks each source and destination immediately before replacing it.
// Existing corrupt bytes are retained as .corrupted.N hard links, while a
// verified temporary replacement is atomically published over the original name.
func Apply(log Log, currentRoots []string, preview bool, writer io.Writer) error {
	if writer == nil {
		return errors.New("repair output is required")
	}
	if err := ValidateCurrentState(log, currentRoots); err != nil {
		return err
	}
	for _, repair := range log.Repairs {
		source, err := findSource(repair)
		if err != nil {
			return err
		}
		for _, copy := range repair.Copies {
			destination, err := pathBelow(copy.Root, repair.RelPath)
			if err != nil {
				return err
			}
			actual, err := checksumFile(destination)
			if err != nil {
				return fmt.Errorf("revalidate destination %s: %w", destination, err)
			}
			if !checksumsEqual(actual, repair.Checksum) {
				if !checksumsEqual(actual, copy.ActualChecksum) {
					return fmt.Errorf("destination changed immediately before repair: %s", destination)
				}
				if preview {
					fmt.Fprintln(writer, "preview: repaired file:", destination, "to:", repair.Checksum)
				} else {
					if currentSource, err := checksumFile(source); err != nil || !checksumsEqual(currentSource, repair.Checksum) {
						return fmt.Errorf("source changed immediately before repairing %s", destination)
					}
					backup, err := replaceFromFile(source, destination, repair.Checksum)
					if err != nil {
						return fmt.Errorf("repair file %s: %w", destination, err)
					}
					fmt.Fprintln(writer, "move corrupted file:", destination, "to:", backup)
					fmt.Fprintln(writer, "repaired file:", destination, "to:", repair.Checksum)
				}
			}

			recorded, err := readRecordedChecksum(destination + checksumSuffix)
			if err != nil {
				return err
			}
			if !checksumsEqual(recorded, repair.Checksum) {
				if !checksumsEqual(recorded, copy.RecordedChecksum) {
					return fmt.Errorf("checksum sidecar changed immediately before repair: %s", destination+checksumSuffix)
				}
				if preview {
					fmt.Fprintln(writer, "preview: repaired checksumfile:", destination+checksumSuffix, "to:", repair.Checksum)
				} else {
					backup, err := replaceWithBytes(destination+checksumSuffix, []byte(repair.Checksum))
					if err != nil {
						return fmt.Errorf("repair checksum sidecar %s: %w", destination+checksumSuffix, err)
					}
					fmt.Fprintln(writer, "move corrupted checksumfile:", destination+checksumSuffix, "to:", backup)
					fmt.Fprintln(writer, "repaired checksumfile:", destination+checksumSuffix, "to:", repair.Checksum)
				}
			}
		}
	}
	if !preview {
		if err := ValidateCurrentState(log, currentRoots); err != nil {
			return fmt.Errorf("validate completed repairs: %w", err)
		}
	}
	return nil
}

func findSource(repair Repair) (string, error) {
	for _, copy := range repair.Copies {
		path, err := pathBelow(copy.Root, repair.RelPath)
		if err != nil {
			return "", err
		}
		actual, err := checksumFile(path)
		if err == nil && checksumsEqual(actual, repair.Checksum) {
			return path, nil
		}
	}
	return "", fmt.Errorf("no verified source remains for %s", repair.RelPath)
}

func replaceFromFile(source, destination, expectedChecksum string) (string, error) {
	return replace(destination, expectedChecksum, func(temporary *os.File) error {
		sourceFile, err := os.Open(source)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(temporary, sourceFile)
		closeErr := sourceFile.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func replaceWithBytes(destination string, data []byte) (string, error) {
	return replace(destination, checksumBytes(data), func(temporary *os.File) error {
		_, err := temporary.Write(data)
		return err
	})
}

func replace(destination, expectedChecksum string, populate func(*os.File) error) (backup string, returnErr error) {
	info, err := os.Lstat(destination)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("destination is not a regular file: %s", destination)
	}
	parent := filepath.Dir(destination)
	temporary, err := os.CreateTemp(parent, ".mirror-repair-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	keepTemporary := true
	defer func() {
		if keepTemporary {
			if err := os.Remove(temporaryPath); returnErr == nil && err != nil && !errors.Is(err, os.ErrNotExist) {
				returnErr = err
			}
		}
	}()
	if err := populate(temporary); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Chmod(0o444); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	actual, err := checksumFile(temporaryPath)
	if err != nil {
		return "", err
	}
	if !checksumsEqual(actual, expectedChecksum) {
		return "", fmt.Errorf("temporary replacement checksum is %s, expected %s", actual, expectedChecksum)
	}
	backup, err = NextCorruptedPath(destination)
	if err != nil {
		return "", err
	}
	if err := os.Link(destination, backup); err != nil {
		return "", fmt.Errorf("preserve corrupt file: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return "", err
	}
	keepTemporary = false
	if err := syncDirectory(parent); err != nil {
		return "", err
	}
	return backup, nil
}

func NextCorruptedPath(path string) (string, error) {
	for count := 0; ; count++ {
		candidate := fmt.Sprintf("%s.corrupted.%d", path, count)
		_, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
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
