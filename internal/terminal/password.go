// Package terminal implements cancellable, byte-preserving terminal input.
package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

var ErrInterrupted = errors.New("input interrupted")

const maxPasswordBytes = 4096

// ReadByte polls real file descriptors without a goroutine that could remain
// blocked after cancellation. Non-file readers are used for bounded test input.
func ReadByte(ctx context.Context, reader io.Reader) (byte, error) {
	var one [1]byte
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var n int
		var err error
		if file, ok := reader.(*os.File); ok {
			poll := []unix.PollFd{{Fd: int32(file.Fd()), Events: unix.POLLIN}}
			ready, pollErr := unix.Poll(poll, 100)
			if errors.Is(pollErr, unix.EINTR) {
				continue
			}
			if pollErr != nil {
				return 0, pollErr
			}
			if ready == 0 {
				continue
			}
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			n, err = unix.Read(int(file.Fd()), one[:])
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
				continue
			}
		} else {
			n, err = reader.Read(one[:])
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		if n == 1 {
			return one[0], nil
		}
		if err != nil {
			return 0, err
		}
		return 0, io.EOF
	}
}
func ReadLine(ctx context.Context, reader io.Reader) (string, error) {
	var line []byte
	for {
		b, err := ReadByte(ctx, reader)
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return strings.TrimSuffix(string(line), "\r"), nil
			}
			return "", err
		}
		if b == 3 {
			return "", ErrInterrupted
		}
		if b == '\n' {
			return strings.TrimSuffix(string(line), "\r"), nil
		}
		if len(line) >= 4096 {
			return "", errors.New("input line too long")
		}
		line = append(line, b)
	}
}
func rawTTY() (*os.File, func() error, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	old, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	raw := *old
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(int(f.Fd()), unix.TCSETS, &raw); err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	restore := func() error { return errors.Join(unix.IoctlSetTermios(int(f.Fd()), unix.TCSETS, old), f.Close()) }
	return f, restore, nil
}
func ReadPassword(ctx context.Context, writer io.Writer, prompt string) (password []byte, returnErr error) {
	tty, restore, err := rawTTY()
	if err != nil {
		return nil, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, restore())
		if returnErr != nil {
			clear(password)
			password = nil
		}
	}()
	if _, err := fmt.Fprint(writer, prompt); err != nil {
		return nil, err
	}
	defer func() {
		_, err := fmt.Fprintln(writer)
		returnErr = errors.Join(returnErr, err)
	}()
	return readPassword(ctx, tty)
}
func readPassword(ctx context.Context, reader io.Reader) (password []byte, returnErr error) {
	// Allocate once, and clear the whole allocation on error, including deleted bytes.
	storage := make([]byte, maxPasswordBytes)
	length := 0
	defer func() {
		if returnErr != nil {
			clear(storage)
		}
	}()
	for {
		b, err := ReadByte(ctx, reader)
		if err != nil {
			return nil, err
		}
		switch b {
		case '\r', '\n':
			return storage[:length], nil
		case 3:
			return nil, ErrInterrupted
		case 8, 127:
			if length > 0 {
				length--
				storage[length] = 0
			}
		default:
			if length == len(storage) {
				return nil, errors.New("password is too long")
			}
			storage[length] = b
			length++
		}
	}
}
func PromptByte(ctx context.Context, reader io.Reader) (string, error) {
	b, err := ReadByte(ctx, reader)
	if err != nil {
		return "", err
	}
	if b == 3 {
		return "", ErrInterrupted
	}
	return string([]byte{b}), nil
}
func Confirm(ctx context.Context, writer io.Writer, prompt string) (returnErr error) {
	tty, restore, err := rawTTY()
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, restore()) }()
	if _, err := fmt.Fprintln(writer, prompt+"\nproceed? y/n"); err != nil {
		return err
	}
	choice, err := PromptByte(ctx, tty)
	if err != nil {
		return err
	}
	if choice != "y" {
		return errors.New("repair not confirmed")
	}
	return nil
}
