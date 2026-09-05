package formatdisk

import (
	"context"
	"fmt"
	"time"
)

// These read-only probes have no device/path operands or formatting stdin.
// Options precede the version flag so unsupported required flags fail parsing.
// Inventory preflight separately exercises lsblk/findmnt's actual JSON schema.
// This proves tool startup/option availability, not future kernel or disk success.
func (a *app) preflightTools(ctx context.Context) error {
	for _, command := range [][]string{
		{"lsblk", "--version"},
		{"findmnt", "--version"},
		{"fdisk", "-w", "always", "-W", "always", "--version"},
		{"cryptsetup", "--type", "luks2", "--key-size=512", "--perf-no_read_workqueue", "--perf-no_write_workqueue", "--version"},
		{"mkfs.ext4", "-m", "0", "-V"},
		{"blkid", "-p", "-s", "TYPE", "-o", "value", "--version"},
		{"mount", "--version"},
		{"chown", "--version"},
	} {
		if err := ctx.Err(); err != nil {
			return err
		}
		probe, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := a.system.Output(probe, command[0], command[1:]...)
		cancel()
		if err != nil {
			return fmt.Errorf("required format tool %s failed its read-only startup/option probe; install or fix it before formatting: %w", command[0], err)
		}
	}
	return ctx.Err()
}
