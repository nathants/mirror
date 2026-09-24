package repaircopies

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"mirror/internal/mountdisks"
	"mirror/internal/rootfs"
	"mirror/lib"
)

const repairLogName = "repair-last.jsonl"

// Options supplies the command's inventory, interaction, and journal boundaries.
// Snapshot must freshly resolve the configured mounted mirrors on every call.
// Confirm is required only when applying unfinished safe repairs; it must return
// an error unless the user explicitly approves. Append defaults to Journal.Append
// and must sync each progress record before returning success.
// The caller must hold the operation lock for the entire Run.
type Options struct {
	Snapshot func(context.Context) (mountdisks.Snapshot, error)
	Confirm  func(context.Context) error
	Output   io.Writer
	Append   func(*Journal, Progress) error
}

func Run(ctx context.Context, homeDir string, preview, resume bool, options Options) (returnErr error) {
	if !filepath.IsAbs(homeDir) || filepath.Clean(homeDir) != homeDir {
		return fmt.Errorf("invalid home directory: %q", homeDir)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if options.Snapshot == nil || options.Output == nil {
		return errors.New("repair snapshot and output are required")
	}
	if options.Append == nil {
		options.Append = (*Journal).Append
	}
	logPath := filepath.Join(homeDir, ".mirror", repairLogName)
	if _, err := fmt.Fprintf(options.Output, "repair log: %s\n", logPath); err != nil {
		return err
	}
	var log Log
	var journal *Journal
	record := func(progress Progress) error {
		err := options.Append(journal, progress)
		log = journal.log
		return err
	}
	haveLog := false
	// Registered before guard cleanup: record cleanup failures too, and always
	// distinguish unresolved consensus from operational failure in the journal.
	defer func() {
		if !haveLog {
			return
		}
		if returnErr != nil {
			var err error
			if journal != nil {
				err = record(Progress{Error: returnErr.Error()})
			} else {
				// Before progress starts, only the scan snapshot needs updating.
				log.Error = returnErr.Error()
				err = Save(logPath, log)
			}
			if err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("save stopped repair state (last durable checkpoint may be uncertain): %w", err))
			}
		}
		returnErr = errors.Join(returnErr, PrintSummary(options.Output, log), unresolvedError(log))
		if journal != nil {
			returnErr = errors.Join(returnErr, journal.Close())
		}
	}()
	var roots *rootfs.Set
	defer func() {
		if roots != nil {
			returnErr = errors.Join(returnErr, roots.Close())
		}
	}()
	if resume {
		opened, err := OpenJournal(logPath)
		if err != nil {
			return err
		}
		if opened.log.Status != StatusUnresolved {
			if err := RequirePending(opened.log); err != nil {
				return errors.Join(err, opened.Close())
			}
		}
		journal, log, haveLog = opened, opened.log, true
		if _, err := fmt.Fprintf(options.Output, "loaded %s repair scan completed at %s:\n", log.Status, log.ScannedAt.Format(time.RFC3339)); err != nil {
			return err
		}
	} else {
		state, err := options.Snapshot(ctx)
		if err != nil {
			return err
		}
		started := time.Now().UTC()
		scanning, err := NewScanningLog(started, state.Roots, state.ConfiguredMirrors)
		if err != nil {
			return err
		}
		if err := Save(logPath, scanning); err != nil {
			return err
		}
		log, haveLog = scanning, true
		roots, err = rootfs.OpenSet(ctx, state.Roots)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(options.Output, "scanning %d mounted mirrors (%d configured)...\n", len(state.Roots), state.ConfiguredMirrors); err != nil {
			return err
		}
		scannedCount := 0
		lastReport := time.Now()
		scanCtx, cancel := context.WithCancel(ctx)
		var outputErr error
		observations, err := scan(scanCtx, roots, func(root, path string) {
			scannedCount++
			if outputErr == nil && time.Since(lastReport) >= time.Second {
				_, outputErr = fmt.Fprintf(options.Output, "scanned %d files; latest %s\n", scannedCount, filepath.Join(root, path))
				if outputErr != nil {
					cancel()
				}
				lastReport = time.Now()
			}
		})
		cancel()
		if err := errors.Join(err, outputErr); err != nil {
			return err
		}
		if err := checkSnapshot(ctx, scanning, options.Snapshot); err != nil {
			return err
		}
		completedScan, err := BuildLog(started, time.Now().UTC(), state.Roots, state.ConfiguredMirrors, observations)
		if err != nil && !errors.Is(err, ErrUnresolved) {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := Save(logPath, completedScan); err != nil {
			return err
		}
		log = completedScan
		journal, err = OpenJournal(logPath)
		if err != nil {
			return err
		}
		log = journal.log
		if log.Status == StatusClean {
			_, err := fmt.Fprintln(options.Output, "scan completed: no mismatches")
			return err
		}
	}
	if err := PrintRepairs(options.Output, log); err != nil {
		return err
	}
	if err := checkSnapshot(ctx, log, options.Snapshot); err != nil {
		return err
	}
	if roots == nil {
		var err error
		roots, err = rootfs.OpenSet(ctx, log.Roots)
		if err != nil {
			return err
		}
	}
	pending := false
	for _, repair := range log.Repairs {
		pending = pending || repair.State != RepairCompleted
	}
	if !pending {
		if err := ValidateCurrentState(ctx, log, roots); err != nil {
			return err
		}
	}
	if preview {
		if err := apply(ctx, log, roots, true, options.Output, nil); err != nil {
			return err
		}
		_, err := fmt.Fprintln(options.Output, "preview completed; no repairs applied")
		return err
	}
	if pending {
		if options.Confirm == nil {
			return errors.New("repair confirmation is required")
		}
		if err := options.Confirm(ctx); err != nil {
			return err
		}
		if err := checkSnapshot(ctx, log, options.Snapshot); err != nil {
			return err
		}
		// Validate through the same roots retained for scanning and cleanup.
		if err := ValidateCurrentState(ctx, log, roots); err != nil {
			return err
		}
		guard, err := lib.NewDirectoryGuard(ctx, roots, log.Roots)
		if err != nil {
			return err
		}
		if err := checkSnapshot(ctx, log, options.Snapshot); err != nil {
			return err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			returnErr = errors.Join(returnErr, guard.Relock(cleanup))
		}()
		if err := guard.SetLocked(ctx, false); err != nil {
			return err
		}
		if err := checkSnapshot(ctx, log, options.Snapshot); err != nil {
			return err
		}
		checkpoint := func(i int, state string, failure error) error {
			progress := Progress{Repair: &i, State: state}
			if failure != nil {
				progress.Error = failure.Error()
			}
			if err := record(progress); err != nil {
				return fmt.Errorf("save %s checkpoint for %s: %w", state, log.Repairs[i].RelPath, err)
			}
			return nil
		}
		if err := apply(ctx, log, roots, false, options.Output, checkpoint); err != nil {
			return err
		}
	}
	if err := checkSnapshot(ctx, log, options.Snapshot); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if log.Status == StatusPending {
		completedAt := time.Now().UTC()
		if err := record(Progress{FinishAt: &completedAt}); err != nil {
			return err
		}
	}
	return nil
}
func checkSnapshot(ctx context.Context, log Log, snapshot func(context.Context) (mountdisks.Snapshot, error)) error {
	state, err := snapshot(ctx)
	if err != nil {
		return err
	}
	if state.ConfiguredMirrors != log.ConfiguredMirrors || !equalStrings(canonicalStrings(state.Roots), log.Roots) {
		return errors.New("configured mirror count or mounted roots changed since the logged scan")
	}
	return ctx.Err()
}
