package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoggingUnavailableWithBrokenStderrDoesNotPreventHelp(t *testing.T) {
	child, home, output := testCommand(t, "--help")
	if err := os.WriteFile(filepath.Join(home, ".mirror-logs"), []byte("unavailable log directory"), 0600); err != nil {
		t.Fatal(err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = read.Close()
	defer func() { _ = write.Close() }()
	child.Stderr = write
	if err := child.Run(); err != nil || !strings.Contains(output.String(), "Usage: mirror") {
		t.Fatalf("logging warning prevented help: %v, %q", err, output.String())
	}
}
