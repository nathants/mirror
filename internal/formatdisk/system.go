package formatdisk

import (
	"context"
	"io"
	"os"
	"time"
)

// System is the narrow operating-system boundary used by the formatter. Tests
// replace it so the production state machine can be exercised without exposing
// any host block device.
type System interface {
	Output(context.Context, string, ...string) ([]byte, error)
	Run(context.Context, io.Reader, io.Writer, io.Writer, string, ...string) error
	Lstat(string) (os.FileInfo, error)
	DirectoryEmpty(string) (bool, error)
	MkdirAll(string, os.FileMode) error
	Sleep(context.Context, time.Duration) error
}
