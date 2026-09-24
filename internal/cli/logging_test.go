package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type commandRecord struct {
	Kind     string   `json:"kind"`
	Args     []string `json:"args"`
	Stream   string   `json:"stream"`
	Text     string   `json:"text"`
	ExitCode *int     `json:"exit_code"`
}

func readCommandRecords(t *testing.T, home string) []commandRecord {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(home, ".mirror-logs", "*.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("expected one command log, got %v: %v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var records []commandRecord
	for {
		var record commandRecord
		if err := decoder.Decode(&record); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func TestRunLogsHelpAndParseFailures(t *testing.T) {
	for _, name := range []string{"SUDO_UID", "SUDO_GID", "SUDO_USER"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name string
		args []string
		code int
	}{
		{name: "help", args: []string{"--help"}},
		{name: "command help", args: []string{"mount", "--help"}},
		{name: "missing command", code: 2},
		{name: "unknown command", args: []string{"not-a-command"}, code: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), test.args, strings.NewReader("private input\n"), &stdout, &stderr)
			if code != test.code {
				t.Fatalf("exit code %d, want %d: %s", code, test.code, &stderr)
			}
			records := readCommandRecords(t, home)
			if len(records) < 3 || records[0].Kind != "start" {
				t.Fatalf("missing invocation start/output: %+v", records)
			}
			finish := records[len(records)-1]
			if finish.Kind != "finish" || finish.ExitCode == nil || *finish.ExitCode != code {
				t.Fatalf("missing exit status: %+v", finish)
			}
			var loggedOut, loggedErr strings.Builder
			for _, record := range records {
				switch record.Stream {
				case "stdout":
					loggedOut.WriteString(record.Text)
				case "stderr":
					loggedErr.WriteString(record.Text)
				}
				if strings.Contains(record.Text, "private input") {
					t.Fatal("stdin was logged")
				}
			}
			if loggedOut.String() != stdout.String() || loggedErr.String() != stderr.String() {
				t.Fatalf("log does not match command output: stdout %q / %q; stderr %q / %q", loggedOut.String(), stdout.String(), loggedErr.String(), stderr.String())
			}
		})
	}
}

func logTestPlatform(home string) platform {
	account := &user.User{Uid: strconv.Itoa(os.Geteuid()), Gid: strconv.Itoa(os.Getegid()), Username: "test", HomeDir: home}
	p := fakePlatform(0, map[string]string{"SUDO_UID": account.Uid, "SUDO_GID": account.Gid, "SUDO_USER": account.Username}, account)
	p.effectiveGID = os.Getegid
	p.homeDir = func() (string, error) { return home, nil }
	return p
}

func TestEveryCommandLogsHandlerOutputAndOutcome(t *testing.T) {
	for _, args := range [][]string{
		{"format"}, {"mount"}, {"unmount"},
		{"ensure-copies", "--preview"}, {"repair-copies", "--preview", "--resume"},
		{"lock"}, {"unlock"},
	} {
		t.Run(args[0], func(t *testing.T) {
			home := t.TempDir()
			var stdout, stderr bytes.Buffer
			called := false
			code := runWithLogging(context.Background(), args, strings.NewReader("private input"), &stdout, &stderr, logTestPlatform(home), func(out, errOut io.Writer) handlers {
				run := func() error {
					called = true
					_, outErr := fmt.Fprintln(out, "planned work")
					_, stderrErr := fmt.Fprintln(errOut, "diagnostic")
					return errors.Join(outErr, stderrErr, errors.New("operation failure"))
				}
				return handlers{
					format:       func(context.Context, callerIdentity) error { return run() },
					mount:        func(context.Context, callerIdentity) error { return run() },
					unmount:      func(context.Context, callerIdentity) error { return run() },
					ensureCopies: func(context.Context, bool) error { return run() },
					repairCopies: func(context.Context, bool, bool) error { return run() },
					lock:         func(context.Context) error { return run() },
					unlock:       func(context.Context) error { return run() },
				}
			})
			if code != 1 || !called || stdout.String() != "planned work\n" || stderr.String() != "diagnostic\nfatal: operation failure\n" {
				t.Fatalf("changed command result: %d, called=%t, %q / %q", code, called, &stdout, &stderr)
			}
			records := readCommandRecords(t, home)
			if len(records) != 5 || !reflect.DeepEqual(records[0].Args, args) || records[1].Text != stdout.String() || records[2].Text+records[3].Text != stderr.String() {
				t.Fatalf("lost command diagnostics: %+v", records)
			}
			if records[4].ExitCode == nil || *records[4].ExitCode != code {
				t.Fatalf("lost failure status: %+v", records[4])
			}
		})
	}
}

func TestLogUsesValidatedSudoHomeForCommandsAndHelp(t *testing.T) {
	for _, args := range [][]string{{"mount"}, {"--help"}, {"lock"}} {
		t.Run(args[0], func(t *testing.T) {
			home, wrongHome := t.TempDir(), t.TempDir()
			p := logTestPlatform(home)
			p.homeDir = func() (string, error) { return wrongHome, nil }
			var stdout, stderr bytes.Buffer
			code := runWithLogging(context.Background(), args, strings.NewReader(""), &stdout, &stderr, p, func(io.Writer, io.Writer) handlers { return noOpHandlers() })
			if code != 0 || stderr.Len() != 0 {
				t.Fatalf("logging changed exit status: %d, %s", code, &stderr)
			}
			records := readCommandRecords(t, home)
			if records[0].Kind != "start" || records[len(records)-1].Kind != "finish" {
				t.Fatalf("silent command lost lifecycle: %+v", records)
			}
			entries, err := os.ReadDir(wrongHome)
			if err != nil || len(entries) != 0 {
				t.Fatalf("used root/environment HOME instead of caller home: %v, %v", entries, err)
			}
		})
	}
}

func TestLoggingDoesNotTrustInvalidSudoMetadataOrFallBackToHOME(t *testing.T) {
	home := t.TempDir()
	p := logTestPlatform(home)
	p.lookupEnv = func(name string) (string, bool) { return "unverified", name == "SUDO_UID" }
	var stdout, stderr bytes.Buffer
	code := runWithLogging(context.Background(), []string{"--help"}, strings.NewReader(""), &stdout, &stderr, p, func(io.Writer, io.Writer) handlers { return noOpHandlers() })
	if code != 0 || !strings.Contains(stdout.String(), "Usage: mirror") || !strings.Contains(stderr.String(), "warning: command logging unavailable") {
		t.Fatalf("help did not continue with a warning: %d, %q / %q", code, &stdout, &stderr)
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("wrote using unverifiable identity: %v, %v", entries, err)
	}
}

func TestUnavailableLoggingDoesNotChangeCommandOutcome(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".mirror-logs")
			if err := os.WriteFile(path, []byte("existing data"), 0600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			called := false
			code := runWithLogging(context.Background(), []string{"lock"}, strings.NewReader(""), &stdout, &stderr, logTestPlatform(home), func(out, _ io.Writer) handlers {
				return handlers{lock: func(context.Context) error {
					called = true
					if _, err := fmt.Fprintln(out, "command output"); err != nil {
						return err
					}
					if failed {
						return errors.New("operation failed")
					}
					return nil
				}}
			})
			if !called || (code != 0) != failed || stdout.String() != "command output\n" || !strings.Contains(stderr.String(), "warning: command logging unavailable") {
				t.Fatalf("logging failure changed command behavior: %d, called=%t, %q / %q", code, called, &stdout, &stderr)
			}
			if strings.Contains(stderr.String(), "fatal: operation failed") != failed {
				t.Fatalf("lost operational error: %s", &stderr)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "existing data" {
				t.Fatalf("changed colliding log path: %q, %v", data, err)
			}
		})
	}
}
