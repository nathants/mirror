package e2e

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Device-free tests of the same runners used by the opt-in VM lifecycle.
func TestRunnerHelper(t *testing.T) {
	mode := os.Getenv("MIRROR_RUNNER_HELPER")
	if mode == "" {
		return
	}
	if mode == "sleep" {
		if err := os.WriteFile(os.Getenv("MIRROR_RUNNER_READY"), nil, 0600); err != nil {
			os.Exit(90)
		}
		time.Sleep(3 * time.Second)
		os.Exit(0)
	}
	if mode != "before" && mode != "output" {
		// Split prompt writes and repeated prompts exercise retained search state.
		input := bufio.NewReader(os.Stdin)
		for _, answer := range []string{"one", "two"} {
			fmt.Fprint(os.Stdout, "pro")
			time.Sleep(5 * time.Millisecond)
			fmt.Fprint(os.Stderr, "mpt:")
			line, err := input.ReadString('\n')
			if err != nil || line != answer+"\n" {
				os.Exit(91)
			}
		}
		fmt.Fprintln(os.Stdout, "answers-ok")
		if mode == "success" {
			os.Exit(0)
		}
	}
	child := exec.Command(os.Args[0], "-test.run=^TestRunnerHelper$")
	child.Env = append(os.Environ(), "MIRROR_RUNNER_HELPER=sleep")
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if child.Start() != nil {
		os.Exit(92)
	}
	_ = child.Wait()
	os.Exit(0)
}

func TestInteractiveRunnerAnswersPrompts(t *testing.T) {
	t.Setenv("MIRROR_RUNNER_HELPER", "success")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := interactiveOutput(ctx, t.TempDir(), os.Args[0], []interaction{{"prompt:", "one"}, {"prompt:", "two"}}, "-test.run=^TestRunnerHelper$")
	if err != nil || !strings.Contains(string(output), "answers-ok") {
		t.Fatalf("interaction: %v; output %q", err, output)
	}
}

func TestRunnersCancelInheritedPipes(t *testing.T) {
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	for _, mode := range []string{"before", "after", "output"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("MIRROR_RUNNER_HELPER", mode)
			ready := filepath.Join(t.TempDir(), "ready")
			t.Setenv("MIRROR_RUNNER_READY", ready)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			watchDone := make(chan bool, 1)
			go func() {
				for {
					if _, err := os.Stat(ready); err == nil {
						cancel()
						watchDone <- true
						return
					}
					select {
					case <-ctx.Done():
						watchDone <- false
						return
					case <-time.After(5 * time.Millisecond):
					}
				}
			}()
			started := time.Now()
			var err error
			if mode == "output" {
				_, err = commandOutput(ctx, t.TempDir(), nil, nil, os.Args[0], "-test.run=^TestRunnerHelper$")
			} else {
				_, err = interactiveOutput(ctx, t.TempDir(), os.Args[0], []interaction{{"prompt:", "one"}, {"prompt:", "two"}}, "-test.run=^TestRunnerHelper$")
			}
			cancel()
			if !<-watchDone {
				t.Fatal("helper never reached cancellation point")
			}
			if err == nil || time.Since(started) > time.Second {
				t.Fatalf("cancellation took %s: %v", time.Since(started), err)
			}
		})
	}
}
