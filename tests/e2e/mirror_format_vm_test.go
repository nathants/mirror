package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"mirror/internal/system"
)

func TestMirrorFormatVM(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("the VM formatter test currently builds Linux amd64 guest tools")
	}
	if os.Getenv("MIRROR_E2E_VM") != "1" {
		t.Skip("set MIRROR_E2E_VM=1 to run the real-tool VM test")
	}
	if _, err := exec.LookPath("vm"); err != nil {
		t.Fatalf("vm command is unavailable: %v", err)
	}

	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate E2E test source")
	}
	repo := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "../.."))
	work := t.TempDir()
	mirror := filepath.Join(work, "mirror")
	checker := filepath.Join(work, "vmcheck")
	buildEnvironment := overrideEnvironment(map[string]string{
		"CGO_ENABLED": "0",
		"GOOS":        "linux",
		"GOARCH":      "amd64",
	})
	run(t, repo, buildEnvironment, nil, "go", "build", "-trimpath", "-o", mirror, "./cmd/mirror")
	run(t, repo, buildEnvironment, nil, "go", "build", "-trimpath", "-o", checker, "./tests/e2e/vmcheck")

	name := "mirror-e2e-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	keep := os.Getenv("MIRROR_E2E_KEEP") == "1"
	created := true
	t.Cleanup(func() {
		if !created || keep {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		output, err := commandOutput(ctx, repo, nil, nil, "vm", "rm", name)
		if err != nil {
			t.Errorf("clean up VM %s: %v\n%s", name, err, output)
		}
	})

	started := time.Now()
	newArgs := []string{"new", "--image", "mirror-e2e", "--mem", "1024", "--cpus", "2", "--disk", "8", "--data-disk", "1"}
	if keep {
		newArgs = append(newArgs, "--no-ephemeral")
	}
	newArgs = append(newArgs, name)
	run(t, repo, nil, nil, "vm", newArgs...)
	run(t, repo, nil, nil, "vm", "wait-ssh", "--timeout", "120", name)
	run(t, repo, nil, nil, "vm", "scp", name, mirror, ":/tmp/mirror")
	run(t, repo, nil, nil, "vm", "scp", name, checker, ":/tmp/vmcheck")
	target := strings.TrimSpace(string(run(t, repo, nil, nil, "vm", "ssh", "--cmd", "sudo -n -- /tmp/vmcheck target", name)))
	if !regexp.MustCompile(`^/dev/sd[a-z]+$`).MatchString(target) {
		t.Fatalf("guest checker returned an invalid target path: %q", target)
	}
	run(t, repo, nil, nil, "vm", "ssh", "--cmd", "sudo -n -- /tmp/vmcheck preflight "+target, name)

	const password = "mirror-e2e-pass"
	output := runInteractive(t, repo, "vm", []interaction{
		{prompt: "which whole disk do you want to reformat (and completely erase)?", response: target},
		{prompt: "Type the exact device path (" + target + ") to confirm:", response: target},
		{prompt: "Are you sure? (Type 'yes' in capital letters):", response: "YES"},
		{prompt: "Enter passphrase for " + target + "1:", response: password},
		{prompt: "Verify passphrase:", response: password},
		{prompt: "Enter passphrase for " + target + "1:", response: password},
	}, "ssh", "--tty", "--cmd", "sudo -n -- /tmp/mirror format", name)
	if bytes.Contains(output, []byte(password)) {
		t.Fatal("formatter interaction echoed the test passphrase")
	}
	if !bytes.Contains(output, []byte("formatted and mounted "+target)) {
		t.Fatalf("formatter did not report success:\n%s", output)
	}
	run(t, repo, nil, nil, "vm", "ssh", "--cmd", "sudo -n -- /tmp/vmcheck postcheck "+target, name)
	run(t, repo, nil, nil, "vm", "ssh", "--cmd", "/tmp/vmcheck write-probe "+target, name)
	run(t, repo, nil, nil, "vm", "ssh", "--cmd", "sudo -n -- /tmp/vmcheck configure "+target, name)
	output = runInteractive(t, repo, "vm", []interaction{
		{prompt: "type \"unmount\" to confirm:", response: "unmount"},
	}, "ssh", "--tty", "--cmd", "sudo -n -- /tmp/mirror unmount", name)
	if !bytes.Contains(output, []byte("closed /dev/mapper/"+filepath.Base(target))) {
		t.Fatalf("unmount did not report success:\n%s", output)
	}
	run(t, repo, nil, nil, "vm", "ssh", "--cmd", "sudo -n -- /tmp/vmcheck postunmount "+target, name)

	output = runInteractive(t, repo, "vm", []interaction{
		{prompt: "type \"mount\" to confirm:", response: "mount"},
		{prompt: "disk password:", response: password},
	}, "ssh", "--tty", "--cmd", "sudo -n -- /tmp/mirror mount", name)
	if bytes.Contains(output, []byte(password)) {
		t.Fatal("mount interaction echoed the test passphrase")
	}
	if !bytes.Contains(output, []byte("mounted "+target+"1 at /home/arch/mnt."+filepath.Base(target))) {
		t.Fatalf("mount did not report success:\n%s", output)
	}
	run(t, repo, nil, nil, "vm", "ssh", "--cmd", "sudo -n -- /tmp/vmcheck postcheck "+target, name)
	run(t, repo, nil, nil, "vm", "ssh", "--cmd", "/tmp/vmcheck verify-probe "+target, name)
	output = runInteractive(t, repo, "vm", []interaction{
		{prompt: "type \"unmount\" to confirm:", response: "unmount"},
	}, "ssh", "--tty", "--cmd", "sudo -n -- /tmp/mirror unmount", name)
	if !bytes.Contains(output, []byte("closed /dev/mapper/"+filepath.Base(target))) {
		t.Fatalf("final unmount did not report success:\n%s", output)
	}
	run(t, repo, nil, nil, "vm", "ssh", "--cmd", "sudo -n -- /tmp/vmcheck postunmount "+target, name)

	if keep {
		t.Logf("retained VM %s", name)
	} else {
		run(t, repo, nil, nil, "vm", "stop", name)
		created = false
	}
	t.Logf("production mirror format, mount, and unmount commands completed a real-tool lifecycle on an isolated secondary VM disk in %s", time.Since(started).Round(time.Millisecond))
}

type interaction struct {
	prompt   string
	response string
}

func runInteractive(t *testing.T, directory string, name string, interactions []interaction, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	output, err := interactiveOutput(ctx, directory, name, interactions, args...)
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, output)
	}
	return output
}

func interactiveOutput(ctx context.Context, directory, name string, interactions []interaction, args ...string) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	input, answers, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() { _ = input.Close() }()
	defer func() { _ = answers.Close() }()
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = answers.Close() // unblock a response write if the command stops reading
		close(closed)
	})
	defer func() {
		if !stop() {
			<-closed
		}
	}()
	output := &interactionOutput{answers: answers, interactions: interactions, cancel: cancel}
	command := exec.Command(name, args...)
	command.Dir = directory
	command.Stdin = input
	// The same Writer makes os/exec serialize both streams through one copier.
	// RunCommand owns Wait and bounds pipe draining even before the next prompt.
	command.Stdout, command.Stderr = output, output
	err = system.RunCommand(ctx, command)
	if err == nil && output.next != len(interactions) {
		err = fmt.Errorf("command ended before prompt %q", interactions[output.next].prompt)
	}
	return output.buffer.Bytes(), err
}

type interactionOutput struct {
	buffer       bytes.Buffer
	answers      *os.File
	interactions []interaction
	cancel       context.CancelFunc
	next         int
	searchFrom   int
}

func (out *interactionOutput) Write(data []byte) (int, error) {
	n, _ := out.buffer.Write(data)
	for out.next < len(out.interactions) {
		exchange := out.interactions[out.next]
		match := bytes.Index(out.buffer.Bytes()[out.searchFrom:], []byte(exchange.prompt))
		if match < 0 {
			break
		}
		out.searchFrom += match + len(exchange.prompt)
		if _, err := io.WriteString(out.answers, exchange.response+"\n"); err != nil {
			out.cancel()
			return n, fmt.Errorf("answer prompt %q: %w", exchange.prompt, err)
		}
		out.next++
	}
	return n, nil
}

func run(t *testing.T, directory string, environment []string, stdin io.Reader, name string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	output, err := commandOutput(ctx, directory, environment, stdin, name, args...)
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, output)
	}
	return output
}

func commandOutput(ctx context.Context, directory string, environment []string, stdin io.Reader, name string, args ...string) ([]byte, error) {
	command := exec.Command(name, args...)
	command.Dir = directory
	if environment != nil {
		command.Env = environment
	}
	command.Stdin = stdin
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	err := system.RunCommand(ctx, command)
	return output.Bytes(), err
}

func overrideEnvironment(overrides map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if _, replaced := overrides[name]; !replaced {
			environment = append(environment, value)
		}
	}
	for name, value := range overrides {
		environment = append(environment, fmt.Sprintf("%s=%s", name, value))
	}
	return environment
}
