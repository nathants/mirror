package terminal

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestReadPromptByteReturnsInterruptionInsteadOfExiting(t *testing.T) {
	_, err := PromptByte(context.Background(), strings.NewReader("\x03"))
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("error = %v, want ErrInterrupted", err)
	}
}

func TestReadPromptByteReturnsChoice(t *testing.T) {
	choice, err := PromptByte(context.Background(), strings.NewReader("y"))
	if err != nil {
		t.Fatal(err)
	}
	if choice != "y" {
		t.Fatalf("choice = %q, want y", choice)
	}
}

func TestReadLineCancellationClosesNoCallerDescriptor(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := ReadLine(ctx, reader); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("line input ignored cancellation")
	}
	if _, err := writer.Write([]byte("ok\n")); err != nil {
		t.Fatal("cancellation closed caller stdin")
	}
	line, err := ReadLine(context.Background(), reader)
	if err != nil || line != "ok" {
		t.Fatalf("read after cancellation: %q %v", line, err)
	}
}
func TestPasswordPreservesBytesAndEdits(t *testing.T) {
	password, err := readPassword(context.Background(), strings.NewReader("secret  phrase * -nX\x7f\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(password)
	if string(password) != "secret  phrase * -n" {
		t.Fatalf("password bytes: %q", password)
	}
}
