// Package cancelio checks cancellation between bounded I/O operations.
package cancelio

import (
	"context"
	"io"
)

type Reader struct {
	Context context.Context
	Reader  io.Reader
}

func (r Reader) Read(p []byte) (int, error) {
	if err := r.Context.Err(); err != nil {
		return 0, err
	}
	n, err := r.Reader.Read(p)
	if ctxErr := r.Context.Err(); ctxErr != nil {
		return n, ctxErr
	}
	return n, err
}
func Copy(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	return io.CopyBuffer(dst, Reader{Context: ctx, Reader: src}, make([]byte, 128*1024))
}
