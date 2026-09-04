package repaircopies

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"mirror/internal/mountdisks"
	"mirror/internal/terminal"
	"mirror/lib"
)

const repairLogName = "repair-last.json"

type commandOptions struct {
	snapshot func(context.Context) (mountdisks.Snapshot, error)
	confirm  func(context.Context) error
	output   io.Writer
}

func Run(ctx context.Context, homeDir string, preview, resume bool) error {
	return run(ctx, homeDir, preview, resume, commandOptions{
		snapshot: func(ctx context.Context) (mountdisks.Snapshot, error) { return lib.MountedMirrorState(ctx, homeDir) },
		confirm: func(ctx context.Context) error {
			return terminal.Confirm(ctx, os.Stdout, "going to repair these files from the mirror.")
		},
		output: os.Stdout,
	})
}
func run(ctx context.Context, homeDir string, preview, resume bool, options commandOptions) (returnErr error) {
	if !filepath.IsAbs(homeDir) || filepath.Clean(homeDir) != homeDir {
		return fmt.Errorf("invalid home directory: %q", homeDir)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	logPath := filepath.Join(homeDir, ".mirror", repairLogName)
	var log Log
	var err error
	if resume {
		log, err = Load(logPath)
		if err != nil {
			return err
		}
		if err := RequirePending(log); err != nil {
			return err
		}
		fmt.Fprintf(options.output, "loaded pending repair scan completed at %s:\n", log.ScannedAt.Format(time.RFC3339))
	} else {
		state, err := options.snapshot(ctx)
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
		fmt.Fprintf(options.output, "scanning %d mounted mirrors (%d configured)...\n", len(state.Roots), state.ConfiguredMirrors)
		scannedCount := 0
		lastReport := time.Now()
		observations, err := scan(ctx, state.Roots, func(root, path string) {
			scannedCount++
			if time.Since(lastReport) >= time.Second {
				fmt.Fprintf(options.output, "scanned %d files; latest %s\n", scannedCount, filepath.Join(root, path))
				lastReport = time.Now()
			}
		})
		if err != nil {
			return err
		}
		if err := checkSnapshot(ctx, scanning, options.snapshot); err != nil {
			return err
		}
		log, err = BuildLog(started, time.Now().UTC(), state.Roots, state.ConfiguredMirrors, observations)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := Save(logPath, log); err != nil {
			return err
		}
		if log.Status == StatusClean {
			fmt.Fprintln(options.output, "scan completed: no mismatches")
			return nil
		}
	}
	if err := PrintRepairs(options.output, log); err != nil {
		return err
	}
	if err := checkSnapshot(ctx, log, options.snapshot); err != nil {
		return err
	}
	if preview {
		if err := Apply(ctx, log, log.Roots, true, options.output); err != nil {
			return err
		}
		fmt.Fprintln(options.output, "preview completed; repair log remains pending")
		return nil
	}
	if err := options.confirm(ctx); err != nil {
		return err
	}
	if err := checkSnapshot(ctx, log, options.snapshot); err != nil {
		return err
	}
	// Pin roots before unlocking and keep ownership until cleanup finishes.
	// Revalidate inventory after pinning; cleanup must never reopen root paths.
	if err := ValidateCurrentState(ctx, log, log.Roots); err != nil {
		return err
	}
	guard, err := lib.OpenDirectoryGuard(ctx, log.Roots)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, guard.Close()) }()
	if err := checkSnapshot(ctx, log, options.snapshot); err != nil {
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
	if err := checkSnapshot(ctx, log, options.snapshot); err != nil {
		return err
	}
	if err := Apply(ctx, log, log.Roots, false, options.output); err != nil {
		return err
	}
	if err := checkSnapshot(ctx, log, options.snapshot); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	completed, err := MarkCompleted(log, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := Save(logPath, completed); err != nil {
		return err
	}
	fmt.Fprintln(options.output, "repair completed")
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
