package terminal

import (
	"errors"
	"fmt"
	"io"

	"github.com/pkg/term"
)

var ErrInterrupted = errors.New("input interrupted")

const maxPasswordBytes = 4096

// ReadPassword reads one byte-preserving, non-echoed line from /dev/tty.
func ReadPassword(writer io.Writer, prompt string) (password []byte, returnErr error) {
	terminal, err := term.Open("/dev/tty")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := terminal.Close(); returnErr == nil && err != nil {
			returnErr = err
		}
	}()
	if err := term.RawMode(terminal); err != nil {
		return nil, err
	}
	defer func() {
		if err := terminal.Restore(); returnErr == nil && err != nil {
			returnErr = err
		}
	}()

	if _, err := fmt.Fprint(writer, prompt); err != nil {
		return nil, err
	}
	defer fmt.Fprintln(writer)

	var one [1]byte
	for {
		count, err := terminal.Read(one[:])
		if count == 1 {
			switch one[0] {
			case '\r', '\n':
				return password, nil
			case 3:
				clear(password)
				return nil, ErrInterrupted
			case 8, 127:
				if len(password) > 0 {
					password = password[:len(password)-1]
				}
			default:
				if len(password) >= maxPasswordBytes {
					clear(password)
					return nil, errors.New("password is too long")
				}
				password = append(password, one[0])
			}
		}
		if err != nil {
			clear(password)
			return nil, err
		}
		if count == 0 {
			clear(password)
			return nil, io.ErrNoProgress
		}
	}
}
