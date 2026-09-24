package commandlog

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Warn writes a best-effort diagnostic without recording it. For file outputs,
// a temporary descriptor above stderr avoids Go's fatal SIGPIPE handling for
// writes to descriptors 1 and 2. Normal command output and process-wide signal
// behavior are unchanged. If even duplicating the descriptor fails, drop the
// warning rather than allowing a debug-log failure to terminate storage work.
func Warn(output io.Writer, err error) {
	output = Unwrap(output)
	if file, ok := output.(*os.File); ok {
		conn, connErr := file.SyscallConn()
		if connErr != nil {
			return
		}
		var fd int
		var dupErr error
		if controlErr := conn.Control(func(original uintptr) {
			fd, dupErr = unix.FcntlInt(original, unix.F_DUPFD_CLOEXEC, 3)
		}); controlErr != nil || dupErr != nil {
			return
		}
		duplicate := os.NewFile(uintptr(fd), file.Name())
		defer duplicate.Close()
		output = duplicate
	}
	fmt.Fprintln(output, "warning:", err)
}
