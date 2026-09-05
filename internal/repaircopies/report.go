package repaircopies

import (
	"errors"
	"fmt"
	"io"
)

func PrintRepairs(writer io.Writer, log Log) error {
	if writer == nil {
		return errors.New("repair output is required")
	}
	if err := validateLog(log); err != nil {
		return err
	}
	for _, finding := range log.Unresolved {
		if _, err := fmt.Fprintf(writer, "unresolved (scan evidence; not a repair target): %s\n  reason: %s\n", finding.RelPath, finding.Reason); err != nil {
			return err
		}
		if err := printCopies(writer, finding.Copies); err != nil {
			return err
		}
	}
	for _, repair := range log.Repairs {
		if _, err := fmt.Fprintf(writer, "repairable (%s): %s\n", repair.State, repair.RelPath); err != nil {
			return err
		}
		if err := printCopies(writer, repair.Copies); err != nil {
			return err
		}
	}
	return nil
}

func printCopies(writer io.Writer, copies []Copy) error {
	for _, copy := range copies {
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
		if _, err := fmt.Fprintf(writer, "  disk: %s\n    sidecar: %s\n    actual:  %s\n", copy.Root, value, actual); err != nil {
			return err
		}
	}
	return nil
}

// PrintSummary describes last durably recorded verification, not a guarantee
// that the filesystem has stayed unchanged since then. Started is uncertain.
func PrintSummary(writer io.Writer, log Log) error {
	if writer == nil {
		return errors.New("repair output is required")
	}
	if err := validateLog(log); err != nil {
		return err
	}
	if log.Status == StatusScanning {
		_, err := fmt.Fprintf(writer, "scan incomplete; completed findings unavailable\noperation stopped: %s\n", log.Error)
		return err
	}
	repaired, failed, pending := 0, 0, 0
	for _, repair := range log.Repairs {
		switch repair.State {
		case RepairCompleted:
			repaired++
		case RepairStarted, RepairFailed:
			failed++
		case RepairPending:
			pending++
		}
	}
	if _, err := fmt.Fprintf(writer, "repair summary (last recorded state): repaired=%d unresolved=%d failed/uncertain=%d unattempted=%d\n", repaired, len(log.Unresolved), failed, pending); err != nil {
		return err
	}
	for _, repair := range log.Repairs {
		state := repair.State
		if state == RepairPending {
			state = "unattempted"
		} else if state == RepairStarted {
			state = "failed/uncertain (started without a durable result)"
		}
		if _, err := fmt.Fprintf(writer, "  %s: %s\n", state, repair.RelPath); err != nil {
			return err
		}
		if repair.Error != "" {
			if _, err := fmt.Fprintf(writer, "    %s\n", repair.Error); err != nil {
				return err
			}
		}
	}
	for _, finding := range log.Unresolved {
		if _, err := fmt.Fprintf(writer, "  unresolved: %s: %s\n", finding.RelPath, finding.Reason); err != nil {
			return err
		}
	}
	if log.Error != "" {
		if _, err := fmt.Fprintf(writer, "last operational error: %s\n", log.Error); err != nil {
			return err
		}
	}
	if len(log.Unresolved) > 0 {
		_, err := fmt.Fprintln(writer, "unresolved evidence is from the logged scan; inspect it manually, then run a fresh scan after resolution")
		return err
	}
	return nil
}
