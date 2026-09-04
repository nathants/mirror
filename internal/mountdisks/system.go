package mountdisks

import (
	"context"
	"io"
	"os"
)

// System is the operating-system boundary used by mount and unmount workflows.
type System interface {
	Output(context.Context, string, ...string) ([]byte, error)
	Run(context.Context, io.Reader, io.Writer, io.Writer, string, ...string) error
	Lstat(string) (os.FileInfo, error)
	DirectoryEmpty(string) (bool, error)
	MkdirAll(string, os.FileMode) error
}
