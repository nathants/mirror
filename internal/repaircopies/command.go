package repaircopies

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"mirror/internal/mountdisks"
	"mirror/lib"
)

const repairLogName = "repair-last.json"

// Options supplies the command's inventory, interaction, and journal boundaries.
// Snapshot must freshly resolve the configured mounted mirrors on every call.
// Confirm is required only when applying unfinished safe repairs; it must return
// an error unless the user explicitly approves. Save defaults to the atomic Save.
// The caller must hold the operation lock for the entire Run.
type Options struct {
	Snapshot func(context.Context) (mountdisks.Snapshot, error)
	Confirm  func(context.Context) error
	Output   io.Writer
	Save     func(string, Log) error
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
	if options.Save == nil {
		options.Save = Save
	}
	logPath := filepath.Join(homeDir, ".mirror", repairLogName)
	if _, err := fmt.Fprintf(options.Output, "repair log: %s\n", logPath); err != nil {
		return err
	}
	var log Log
	haveLog := false
	// Registered before guard cleanup: record cleanup failures too, and always
	// distinguish unresolved consensus from operational failure in the journal.
	defer func() {
		if !haveLog {
			return
		}
		if returnErr != nil {
			log.Error = returnErr.Error()
			if log.Status == StatusCompleted {
				log.Status = StatusPending
				log.CompletedAt = time.Time{}
			}
			if err := options.Save(logPath, log); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("save stopped repair state (last durable checkpoint may be uncertain): %w", err))
			}
		}
		returnErr = errors.Join(returnErr, PrintSummary(options.Output, log), unresolvedError(log))
	}()
	if resume {
		loaded, err := Load(logPath)
		if err != nil {
			return err
		}
		if loaded.Status != StatusUnresolved {
			if err := RequirePending(loaded); err != nil {
				return err
			}
		}
		log, haveLog = loaded, true
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
		if err := options.Save(logPath, scanning); err != nil {
			return err
		}
		log, haveLog = scanning, true
		if _, err := fmt.Fprintf(options.Output, "scanning %d mounted mirrors (%d configured)...\n", len(state.Roots), state.ConfiguredMirrors); err != nil {
			return err
		}
		scannedCount := 0
		lastReport := time.Now()
		scanCtx, cancel := context.WithCancel(ctx)
		var outputErr error
		observations, err := scan(scanCtx, state.Roots, func(root, path string) {
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
		if err := options.Save(logPath, completedScan); err != nil {
			return err
		}
		log = completedScan
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
	pending := false
	for _, repair := range log.Repairs {
		pending = pending || repair.State != RepairCompleted
	}
	if !pending {
		if err := ValidateCurrentState(ctx, log, log.Roots); err != nil {
			return err
		}
	}
	if preview {
		if err := apply(ctx, log, log.Roots, true, options.Output, nil); err != nil {
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
		// Pin roots before unlocking and keep ownership until cleanup finishes.
		if err := ValidateCurrentState(ctx, log, log.Roots); err != nil {
			return err
		}
		guard, err := lib.OpenDirectoryGuard(ctx, log.Roots)
		if err != nil {
			return err
		}
		defer func() { returnErr = errors.Join(returnErr, guard.Close()) }()
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
			next := log
			next.Repairs = append([]Repair(nil), log.Repairs...)
			next.Error = ""
			next.Repairs[i].State = state
			next.Repairs[i].Error = ""
			if failure != nil {
				next.Repairs[i].Error = failure.Error()
			}
			if err := options.Save(logPath, next); err != nil {
				return fmt.Errorf("save %s checkpoint for %s: %w", state, next.Repairs[i].RelPath, err)
			}
			log = next
			return nil
		}
		if err := apply(ctx, log, log.Roots, false, options.Output, checkpoint); err != nil {
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
		completed, err := MarkCompleted(log, time.Now().UTC())
		if err != nil {
			return err
		}
		if err := options.Save(logPath, completed); err != nil {
			return err
		}
		log = completed
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
