package lib

import (
	"errors"
	"strings"
	"testing"
)

func TestReadPromptByteReturnsInterruptionInsteadOfExiting(t *testing.T) {
	_, err := readPromptByte(strings.NewReader("\x03"))
	if !errors.Is(err, ErrPromptInterrupted) {
		t.Fatalf("error = %v, want ErrPromptInterrupted", err)
	}
}

func TestReadPromptByteReturnsChoice(t *testing.T) {
	choice, err := readPromptByte(strings.NewReader("y"))
	if err != nil {
		t.Fatal(err)
	}
	if choice != "y" {
		t.Fatalf("choice = %q, want y", choice)
	}
}
