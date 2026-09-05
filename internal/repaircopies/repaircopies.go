// Package repaircopies builds, persists, validates, and applies mirror repair plans.
package repaircopies

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/blake2b"
	"mirror/internal/rootfs"
)

const (
	LogVersion      = 3
	StatusScanning  = "scanning"
	StatusPending   = "pending"
	StatusClean     = "clean"
	StatusCompleted = "completed"
	checksumSuffix  = ".b2sum"
)

// Sidecar records a hash of the exact metadata bytes for drift detection and
// an optional canonical checksum for voting. A nil Sidecar means absent.
// Malformed metadata has a Hash but no Value; it never supplies a vote.
type Sidecar struct {
	Hash  string `json:"hash"`
	Value string `json:"value,omitempty"`
}

// Copy records a mirror's evidence. An empty ActualChecksum means the data
// path was absent: its sidecar still votes, but neither path is a repair target.
// Resume and completion require these evidence-only paths to remain unchanged.
type Copy struct {
	Root           string   `json:"root"`
	ActualChecksum string   `json:"actual_checksum"`
	Sidecar        *Sidecar `json:"sidecar"`
}
type Repair struct {
	RelPath  string `json:"path"`
	Checksum string `json:"repair_checksum"`
	Copies   []Copy `json:"copies"`
}
type Log struct {
	Version           int       `json:"version"`
	Status            string    `json:"status"`
	StartedAt         time.Time `json:"started_at"`
	ScannedAt         time.Time `json:"scanned_at"`
	CompletedAt       time.Time `json:"completed_at"`
	Roots             []string  `json:"roots"`
	ConfiguredMirrors int       `json:"configured_mirrors"`
	Repairs           []Repair  `json:"repairs"`
}

func NewScanningLog(startedAt time.Time, roots []string, configured int) (Log, error) {
	log := Log{Version: LogVersion, Status: StatusScanning, StartedAt: startedAt.UTC(), Roots: canonicalStrings(roots), ConfiguredMirrors: configured}
	return log, validateLog(log)
}

// BuildLog counts each mirror at most once per candidate. Exactly one candidate
// must have support from more than half of ALL configured mirrors. Disagreement
// never increases the denominator and multiple qualifying candidates are refused.
func BuildLog(startedAt, scannedAt time.Time, roots []string, configured int, observations map[string][]Copy) (Log, error) {
	log := Log{Version: LogVersion, Status: StatusClean, StartedAt: startedAt.UTC(), ScannedAt: scannedAt.UTC(), Roots: canonicalStrings(roots), ConfiguredMirrors: configured}
	if err := validateLog(log); err != nil {
		return Log{}, err
	}
	paths := make([]string, 0, len(observations))
	for path := range observations {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := validateDataPath(path); err != nil {
			return Log{}, err
		}
		copies := append([]Copy(nil), observations[path]...)
		sort.Slice(copies, func(i, j int) bool { return copies[i].Root < copies[j].Root })
		if err := validateCopies(log.Roots, copies); err != nil {
			return Log{}, fmt.Errorf("%s: %w", path, err)
		}
		clean := true
		dataChecksum := ""
		for _, copy := range copies {
			if copy.ActualChecksum == "" {
				continue // Evidence only; missing data is not a repair target.
			}
			if dataChecksum == "" {
				dataChecksum = copy.ActualChecksum
			}
			if copy.ActualChecksum != dataChecksum || copy.Sidecar == nil || copy.Sidecar.Value != copy.ActualChecksum {
				clean = false
			}
		}
		evidenceDisagrees := false
		for _, copy := range copies {
			if copy.Sidecar != nil && copy.Sidecar.Value != "" && copy.Sidecar.Value != dataChecksum {
				evidenceDisagrees = true
			}
		}
		if clean && dataChecksum != "" && !evidenceDisagrees {
			continue
		}
		winner, err := uniqueWinner(copies, configured)
		if err != nil {
			return Log{}, fmt.Errorf("cannot safely repair %s: %w", path, err)
		}
		if clean && dataChecksum != "" {
			continue // Consensus confirmed existing data; evidence alone is not repaired.
		}
		log.Repairs = append(log.Repairs, Repair{RelPath: path, Checksum: winner, Copies: copies})
	}
	if len(log.Repairs) > 0 {
		log.Status = StatusPending
	}
	return log, validateLog(log)
}
func uniqueWinner(copies []Copy, configured int) (string, error) {
	counts := make(map[string]int)
	for _, copy := range copies {
		if copy.ActualChecksum != "" {
			counts[copy.ActualChecksum]++
		}
		if copy.Sidecar != nil && copy.Sidecar.Value != "" && copy.Sidecar.Value != copy.ActualChecksum {
			counts[copy.Sidecar.Value]++
		}
	}
	winner := ""
	for value, count := range counts {
		if count > configured/2 {
			if winner != "" {
				return "", errors.New("multiple checksums have a strict checksum majority of configured mirrors")
			}
			winner = value
		}
	}
	if winner == "" {
		return "", errors.New("there is no unique strict checksum majority of configured mirrors")
	}
	for _, copy := range copies {
		if copy.ActualChecksum == winner {
			return winner, nil
		}
	}
	return "", errors.New("no data copy matches the majority checksum")
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
			value := "missing"
			if copy.Sidecar != nil {
				value = copy.Sidecar.Value
				if value == "" {
					value = "malformed (bytes hash " + copy.Sidecar.Hash + ")"
				}
			}
			actual := copy.ActualChecksum
			if actual == "" {
				actual = "missing (sidecar evidence only; not a repair target)"
			}
			fmt.Fprintf(writer, "  disk: %s\n    sidecar: %s\n    actual:  %s\n", copy.Root, value, actual)
		}
	}
	return nil
}
func MarkCompleted(log Log, completedAt time.Time) (Log, error) {
	if err := RequirePending(log); err != nil {
		return Log{}, err
	}
	log.Status = StatusCompleted
	log.CompletedAt = completedAt.UTC()
	return log, validateLog(log)
}
func RequirePending(log Log) error {
	if log.Status != StatusPending {
		return fmt.Errorf("last repair scan is %s, not pending", log.Status)
	}
	return nil
}
func validateLog(log Log) error {
	if log.Version != LogVersion {
		return fmt.Errorf("unsupported repair log version: %d; run a fresh scan", log.Version)
	}
	if log.StartedAt.IsZero() || log.StartedAt.Location() != time.UTC {
		return errors.New("repair log has invalid start time")
	}
	if err := validateCanonicalRoots(log.Roots); err != nil {
		return err
	}
	if log.ConfiguredMirrors < len(log.Roots) {
		return errors.New("configured mirror count is smaller than mounted root count")
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
	if !log.ScannedAt.IsZero() && (log.ScannedAt.Location() != time.UTC || log.ScannedAt.Before(log.StartedAt)) {
		return errors.New("repair log scan time is invalid")
	}
	if !log.CompletedAt.IsZero() && (log.CompletedAt.Location() != time.UTC || log.CompletedAt.Before(log.ScannedAt)) {
		return errors.New("repair log completion time is invalid")
	}
	previous := ""
	for i, repair := range log.Repairs {
		if err := validateDataPath(repair.RelPath); err != nil {
			return err
		}
		if i > 0 && repair.RelPath <= previous {
			return errors.New("repair paths are not sorted and unique")
		}
		previous = repair.RelPath
		if !validChecksum(repair.Checksum) {
			return fmt.Errorf("invalid repair checksum for %s", repair.RelPath)
		}
		if err := validateCopies(log.Roots, repair.Copies); err != nil {
			return err
		}
		winner, err := uniqueWinner(repair.Copies, log.ConfiguredMirrors)
		if err != nil || winner != repair.Checksum {
			return fmt.Errorf("repair checksum is not the logged unique strict majority for %s", repair.RelPath)
		}
	}
	return nil
}
func validateCopies(roots []string, copies []Copy) error {
	if len(copies) == 0 {
		return errors.New("observation has no copies")
	}
	previous := ""
	for i, copy := range copies {
		if i > 0 && copy.Root <= previous {
			return errors.New("copy roots are not sorted and unique")
		}
		previous = copy.Root
		if !containsString(roots, copy.Root) {
			return fmt.Errorf("copy root is not in logged roots: %s", copy.Root)
		}
		if copy.ActualChecksum == "" {
			if copy.Sidecar == nil {
				return errors.New("copy has neither data nor sidecar evidence")
			}
		} else if !validChecksum(copy.ActualChecksum) {
			return errors.New("copy has invalid actual checksum")
		}
		if copy.Sidecar != nil {
			if !validChecksum(copy.Sidecar.Hash) {
				return errors.New("invalid sidecar bytes hash")
			}
			if copy.Sidecar.Value != "" && (!validChecksum(copy.Sidecar.Value) || copy.Sidecar.Hash != checksumBytes([]byte(copy.Sidecar.Value))) {
				return errors.New("invalid sidecar value or bytes hash")
			}
		}
	}
	return nil
}
func validateCanonicalRoots(roots []string) error {
	if len(roots) == 0 {
		return errors.New("repair log contains no mirror roots")
	}
	previous := ""
	for i, root := range roots {
		if !utf8.ValidString(root) || strings.ContainsRune(root, 0) || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" {
			return fmt.Errorf("mirror root must be valid UTF-8 and a clean non-root absolute path: %q", root)
		}
		if i > 0 && root <= previous {
			return errors.New("mirror roots are not sorted and unique")
		}
		previous = root
	}
	return nil
}
func validateRelativePath(path string) error {
	if !utf8.ValidString(path) || !rootfs.UserPath(path) {
		return fmt.Errorf("invalid relative path in repair log: %q", path)
	}
	return nil
}

// Payload files and symlinks cannot occupy the checksum metadata namespace.
// Directory components may use the suffix; they are never checksum sidecars.
func validateDataPath(path string) error {
	if err := validateRelativePath(path); err != nil {
		return err
	}
	if strings.HasSuffix(path, checksumSuffix) {
		return fmt.Errorf("invalid payload path %q: .b2sum is reserved for checksum sidecars; rename payload files before ingest", path)
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
func checksumBytes(data []byte) string {
	sum := blake2b.Sum512(data)
	return hex.EncodeToString(sum[:])
}
func canonicalStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}
func containsString(values []string, wanted string) bool {
	i := sort.SearchStrings(values, wanted)
	return i < len(values) && values[i] == wanted
}
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func equalSidecars(a, b *Sidecar) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
