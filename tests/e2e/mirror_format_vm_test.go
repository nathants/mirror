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

	command := exec.CommandContext(ctx, name, args...)
	command.Dir = directory
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatalf("create %s stdin pipe: %v", name, err)
	}
	outputReader, outputWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create %s output pipe: %v", name, err)
	}
	defer outputReader.Close()
	command.Stdout = outputWriter
	command.Stderr = outputWriter
	if err := command.Start(); err != nil {
		outputWriter.Close()
		t.Fatalf("start %s %v: %v", name, args, err)
	}
	if err := outputWriter.Close(); err != nil {
		t.Fatalf("close parent copy of %s output pipe: %v", name, err)
	}

	var output bytes.Buffer
	buffer := make([]byte, 4096)
	searchFrom := 0
	for _, exchange := range interactions {
		for !bytes.Contains(output.Bytes()[searchFrom:], []byte(exchange.prompt)) {
			count, readErr := outputReader.Read(buffer)
			if count > 0 {
				_, _ = output.Write(buffer[:count])
			}
			if readErr != nil {
				_ = stdin.Close()
				waitErr := command.Wait()
				t.Fatalf("%s %v ended before prompt %q: read output: %v; wait: %v\n%s", name, args, exchange.prompt, readErr, waitErr, output.Bytes())
			}
		}
		match := bytes.Index(output.Bytes()[searchFrom:], []byte(exchange.prompt))
		searchFrom += match + len(exchange.prompt)
		if _, err := io.WriteString(stdin, exchange.response+"\n"); err != nil {
			_ = stdin.Close()
			_ = command.Wait()
			t.Fatalf("answer prompt %q from %s %v: %v\n%s", exchange.prompt, name, args, err, output.Bytes())
		}
	}

	_, readErr := io.Copy(&output, outputReader)
	closeErr := stdin.Close()
	waitErr := command.Wait()
	if readErr != nil {
		t.Fatalf("read %s %v output: %v\n%s", name, args, readErr, output.Bytes())
	}
	if closeErr != nil {
		t.Fatalf("close %s %v stdin: %v\n%s", name, args, closeErr, output.Bytes())
	}
	if waitErr != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, waitErr, output.Bytes())
	}
	return output.Bytes()
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
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = directory
	if environment != nil {
		command.Env = environment
	}
	command.Stdin = stdin
	return command.CombinedOutput()
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
