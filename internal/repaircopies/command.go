package repaircopies

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mirror/lib"
)

const repairLogName = "repair-last.json"

type scanResult struct {
	path string
	copy Copy
	done bool
	err  error
}

// Run scans or resumes a scan, presents the repair plan, and applies confirmed repairs.
func Run(homeDir string, preview, resume bool) error {
	if !filepath.IsAbs(homeDir) || filepath.Clean(homeDir) != homeDir {
		return fmt.Errorf("home directory must be a clean absolute path: %q", homeDir)
	}
	logPath := filepath.Join(homeDir, ".mirror", repairLogName)
	if resume {
		return resumeRepair(logPath, preview)
	}
	return scanAndRepair(logPath, preview)
}

func resumeRepair(logPath string, preview bool) error {
	startedAt := time.Now().UTC()
	log, err := Load(logPath)
	if err != nil {
		return fmt.Errorf("load last repair scan: %w", err)
	}
	if err := RequirePending(log); err != nil {
		return err
	}
	fmt.Printf("loaded pending repair scan completed at %s:\n\n", log.ScannedAt.Format(time.RFC3339))
	if err := PrintRepairs(os.Stdout, log); err != nil {
		return err
	}
	if err := lib.PromptProceed("going to repair these files from the logged scan."); err != nil {
		return fmt.Errorf("repair not confirmed: %w", err)
	}

	if preview {
		if err := Apply(log, lib.MountedMirrorRoots(), true, os.Stdout); err != nil {
			return fmt.Errorf("revalidate logged repair plan: %w", err)
		}
		fmt.Printf("\npreview completed in %.1f minutes; repair log remains pending\n", time.Since(startedAt).Minutes())
		return nil
	}

	// Directory unlocking can be a long metadata walk. Do it only after the
	// resumed plan has been presented and confirmed, then revalidate every
	// logged byte before the first repair.
	lib.LockDirs(false)
	defer lib.LockDirs(true)
	if err := Apply(log, lib.MountedMirrorRoots(), false, os.Stdout); err != nil {
		return fmt.Errorf("revalidate and apply logged repair plan: %w", err)
	}
	completed, err := MarkCompleted(log, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := Save(logPath, completed); err != nil {
		return fmt.Errorf("record completed repair: %w", err)
	}
	fmt.Printf("\ntotal runtime: %.1f minutes\n", time.Since(startedAt).Minutes())
	return nil
}

func scanAndRepair(logPath string, preview bool) error {
	startedAt := time.Now().UTC()
	roots := lib.MountedMirrorRoots()
	scanning, err := NewScanningLog(startedAt, roots)
	if err != nil {
		return err
	}
	if err := Save(logPath, scanning); err != nil {
		return fmt.Errorf("record repair scan start: %w", err)
	}

	lib.LockDirs(false)
	defer lib.LockDirs(true)
	disks := lib.ScanDisks()
	observations, err := scanChecksums(disks, time.Now())
	if err != nil {
		return err
	}
	log, err := BuildLog(startedAt, time.Now().UTC(), roots, observations)
	if err != nil {
		return err
	}
	if err := Save(logPath, log); err != nil {
		return fmt.Errorf("record completed repair scan: %w", err)
	}
	if log.Status == StatusClean {
		fmt.Printf("total runtime: %.1f minutes\n", time.Since(startedAt).Minutes())
		return nil
	}

	if err := PrintRepairs(os.Stdout, log); err != nil {
		return err
	}
	if err := lib.PromptProceed("going to repair these files from the mirror."); err != nil {
		return fmt.Errorf("repair not confirmed: %w", err)
	}
	if err := Apply(log, lib.MountedMirrorRoots(), preview, os.Stdout); err != nil {
		return fmt.Errorf("revalidate and apply repair plan: %w", err)
	}
	if preview {
		fmt.Printf("\npreview completed in %.1f minutes; repair log remains pending\n", time.Since(startedAt).Minutes())
		return nil
	}
	completed, err := MarkCompleted(log, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := Save(logPath, completed); err != nil {
		return fmt.Errorf("record completed repair: %w", err)
	}
	fmt.Printf("\ntotal runtime: %.1f minutes\n", time.Since(startedAt).Minutes())
	return nil
}

func scanChecksums(disks map[lib.Disk]map[lib.Path]lib.File, scanStartedAt time.Time) (map[string][]Copy, error) {
	totalCount := 0
	for _, files := range disks {
		for _, file := range files {
			if file.Symlink == nil {
				totalCount++
			}
		}
	}

	results := make(chan scanResult, 128)
	for disk, files := range disks {
		go scanDisk(disk, files, results)
	}
	observations := make(map[string][]Copy)
	workCount := 0
	finished := 0
	for finished < len(disks) {
		result := <-results
		if result.err != nil {
			return nil, result.err
		}
		if result.done {
			finished++
			continue
		}
		workCount++
		if totalCount > 0 {
			elapsed := time.Since(scanStartedAt).Minutes()
			progress := float64(workCount) / float64(totalCount)
			eta := elapsed/progress - elapsed
			fmt.Printf("\r\033[Kscanning: %.1f%% %.1f minutes remaining", progress*100, eta)
		}
		observations[result.path] = append(observations[result.path], result.copy)
	}
	if totalCount > 0 {
		fmt.Print("\r\033[K")
	}
	return observations, nil
}

func scanDisk(disk lib.Disk, files map[lib.Path]lib.File, results chan<- scanResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			results <- scanResult{err: fmt.Errorf("scan %s: %v", disk.AbsPath, recovered)}
		}
		results <- scanResult{done: true}
	}()
	for path, file := range files {
		if file.Symlink != nil {
			continue
		}
		if file.ChecksumFile == nil {
			panic("missing checksum sidecar value for " + path.RelPath)
		}
		absolute := filepath.Join(disk.AbsPath, path.RelPath)
		actual := lib.Blake2bChecksum(absolute)
		results <- scanResult{
			path: path.RelPath,
			copy: Copy{
				Root:             disk.AbsPath,
				ActualChecksum:   actual,
				RecordedChecksum: *file.ChecksumFile,
			},
		}
	}
}
