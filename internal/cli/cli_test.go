package cli

import (
	"bytes"
	"context"
	"fmt"
	"os/user"
	"strings"
	"testing"
)

func noOpHandlers() handlers {
	return handlers{
		format:       func(context.Context, callerIdentity) error { return nil },
		mount:        func(context.Context, callerIdentity) error { return nil },
		unmount:      func(context.Context, callerIdentity) error { return nil },
		ensureCopies: func(bool) error { return nil },
		repairCopies: func(bool, bool) error { return nil },
		lock:         func() error { return nil },
		unlock:       func() error { return nil },
	}
}

func fakePlatform(euid int, environment map[string]string, account *user.User) platform {
	return platform{
		effectiveUID: func() int { return euid },
		lookupEnv: func(name string) (string, bool) {
			value, found := environment[name]
			return value, found
		},
		lookupUserID: func(id string) (*user.User, error) {
			if account == nil {
				return nil, fmt.Errorf("unexpected lookup for %s", id)
			}
			return account, nil
		},
	}
}

func runCLI(arguments []string, platform platform, handlers handlers) (int, string, string) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := execute(context.Background(), arguments, strings.NewReader(""), &stdout, &stderr, platform, handlers)
	return code, stdout.String(), stderr.String()
}

func TestEverySubcommandHasContextualHelp(t *testing.T) {
	for _, command := range []string{
		"format",
		"mount",
		"unmount",
		"ensure-copies",
		"repair-copies",
		"lock",
		"unlock",
	} {
		t.Run(command, func(t *testing.T) {
			handlers := noOpHandlers()
			called := false
			switch command {
			case "unmount":
				handlers.unmount = func(context.Context, callerIdentity) error {
					called = true
					return nil
				}
			case "ensure-copies":
				handlers.ensureCopies = func(bool) error {
					called = true
					return nil
				}
			}
			code, stdout, stderr := runCLI([]string{command, "-h"}, platform{}, handlers)
			if code != 0 {
				t.Fatalf("exit code = %d, stderr = %q", code, stderr)
			}
			if called {
				t.Fatal("help executed the command")
			}
			if !strings.Contains(stdout, "Usage: mirror "+command) {
				t.Fatalf("help is not contextual:\n%s", stdout)
			}
			if stderr != "" {
				t.Fatalf("help wrote stderr: %q", stderr)
			}
		})
	}
}

func TestPrivilegedSubcommandsRejectNonRootBeforeHandler(t *testing.T) {
	for _, command := range []string{"format", "mount", "unmount"} {
		t.Run(command, func(t *testing.T) {
			handlers := noOpHandlers()
			called := false
			markCalled := func(context.Context, callerIdentity) error {
				called = true
				return nil
			}
			switch command {
			case "format":
				handlers.format = markCalled
			case "mount":
				handlers.mount = markCalled
			case "unmount":
				handlers.unmount = markCalled
			}
			code, _, stderr := runCLI([]string{command}, fakePlatform(1000, nil, nil), handlers)
			if code != 1 {
				t.Fatalf("exit code = %d, stderr = %q", code, stderr)
			}
			if called {
				t.Fatal("unprivileged invocation reached command handler")
			}
			if !strings.Contains(stderr, "sudo mirror "+command) {
				t.Fatalf("error does not explain how to invoke command:\n%s", stderr)
			}
		})
	}
}

func TestPrivilegedSubcommandUsesValidatedSudoCaller(t *testing.T) {
	account := &user.User{
		Uid:      "1234",
		Gid:      "5678",
		Username: "alice",
		HomeDir:  "/home/alice",
	}
	environment := map[string]string{
		"SUDO_UID":  "1234",
		"SUDO_GID":  "5678",
		"SUDO_USER": "alice",
	}
	platform := fakePlatform(0, environment, account)
	handlers := noOpHandlers()
	var got callerIdentity
	handlers.mount = func(_ context.Context, caller callerIdentity) error {
		got = caller
		return nil
	}

	code, _, stderr := runCLI([]string{"mount"}, platform, handlers)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	want := callerIdentity{HomeDir: "/home/alice", UID: 1234, GID: 5678}
	if got != want {
		t.Fatalf("caller = %#v, want %#v", got, want)
	}
}

func TestPrivilegedSubcommandsRejectUnverifiableSudoIdentity(t *testing.T) {
	validUser := &user.User{Uid: "1234", Gid: "5678", Username: "alice", HomeDir: "/home/alice"}
	tests := []struct {
		name        string
		environment map[string]string
		account     *user.User
	}{
		{
			name:        "direct root without sudo metadata",
			environment: map[string]string{},
			account:     validUser,
		},
		{
			name: "invalid uid",
			environment: map[string]string{
				"SUDO_UID": "not-a-number", "SUDO_GID": "5678", "SUDO_USER": "alice",
			},
			account: validUser,
		},
		{
			name: "gid does not match passwd",
			environment: map[string]string{
				"SUDO_UID": "1234", "SUDO_GID": "9999", "SUDO_USER": "alice",
			},
			account: validUser,
		},
		{
			name: "username does not match passwd",
			environment: map[string]string{
				"SUDO_UID": "1234", "SUDO_GID": "5678", "SUDO_USER": "mallory",
			},
			account: validUser,
		},
		{
			name: "unsafe home",
			environment: map[string]string{
				"SUDO_UID": "1234", "SUDO_GID": "5678", "SUDO_USER": "alice",
			},
			account: &user.User{Uid: "1234", Gid: "5678", Username: "alice", HomeDir: "relative"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handlers := noOpHandlers()
			called := false
			handlers.format = func(context.Context, callerIdentity) error {
				called = true
				return nil
			}
			code, _, stderr := runCLI(
				[]string{"format"},
				fakePlatform(0, test.environment, test.account),
				handlers,
			)
			if code != 1 || stderr == "" {
				t.Fatalf("exit code = %d, stderr = %q", code, stderr)
			}
			if called {
				t.Fatal("unverifiable sudo identity reached command handler")
			}
		})
	}
}

func TestCopyFlagsDispatchThroughUnifiedCLI(t *testing.T) {
	handlers := noOpHandlers()
	ensurePreview := false
	handlers.ensureCopies = func(preview bool) error {
		ensurePreview = preview
		return nil
	}
	code, _, stderr := runCLI([]string{"ensure-copies", "-p"}, platform{}, handlers)
	if code != 0 || stderr != "" {
		t.Fatalf("ensure-copies exit code = %d, stderr = %q", code, stderr)
	}
	if !ensurePreview {
		t.Fatal("ensure-copies -p did not set preview")
	}

	repairPreview := false
	repairResume := false
	handlers.repairCopies = func(preview, resume bool) error {
		repairPreview = preview
		repairResume = resume
		return nil
	}
	code, _, stderr = runCLI([]string{"repair-copies", "--preview", "--resume"}, platform{}, handlers)
	if code != 0 || stderr != "" {
		t.Fatalf("repair-copies exit code = %d, stderr = %q", code, stderr)
	}
	if !repairPreview || !repairResume {
		t.Fatalf("repair flags = preview %v, resume %v", repairPreview, repairResume)
	}
}
