package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mirror/internal/commandlog"
)

func TestExternalActionStreamsBypassCommandLogging(t *testing.T) {
	t.Setenv("MIRROR_EXEC_HELPER", "echo")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	log, err := commandlog.Open(home, os.Geteuid(), os.Getegid(), []string{"mount"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	out, errOut := log.Writer("stdout", &stdout), log.Writer("stderr", &stderr)
	if _, err := io.WriteString(out, "mirror plan\n"); err != nil {
		t.Fatal(err)
	}
	const secret = "private-input-must-not-be-logged"
	err = (Exec{}).Run(context.Background(), strings.NewReader(secret), out, errOut, os.Args[0], "-test.run=^TestExecHelper$", "--", "literal argument")
	var exit interface{ ExitCode() int }
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("changed action failure: %v", err)
	}
	if stdout.String() != "mirror plan\n"+fmt.Sprintf("%q|C|%s", "literal argument", secret) || stderr.String() != "diagnostic" {
		t.Fatalf("changed external terminal output: %q / %q", &stdout, &stderr)
	}
	// The action's own failure is safe to report; it must not acquire the
	// external stderr stream merely because diagnostics are enabled.
	if _, err := fmt.Fprintln(errOut, "mirror action failed:", err); err != nil {
		t.Fatal(err)
	}
	log.Close(1)
	paths, err := filepath.Glob(filepath.Join(home, ".mirror-logs", "*.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("command logs: %v, %v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{secret, "diagnostic", "|C|"} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatalf("external action output leaked into log: %q", forbidden)
		}
	}
	for _, required := range []string{"mirror plan", "mirror action failed", "exit status 23"} {
		if !bytes.Contains(data, []byte(required)) {
			t.Fatalf("lost mirror diagnostic: %q", required)
		}
	}
}
