// Package cli implements the unified mirror command-line interface.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"

	arg "github.com/alexflint/go-arg"

	"mirror/internal/commandlog"
	"mirror/internal/terminal"
	"mirror/lib"
)

type emptyOptions struct{}

type previewOptions struct {
	Preview bool `arg:"-p,--preview" help:"show planned copies without applying them"`
}

type repairOptions struct {
	Preview bool `arg:"-p,--preview" help:"show planned repairs without applying them"`
	Resume  bool `arg:"--resume" help:"use the last pending completed scan instead of scanning again"`
}

type commandLine struct {
	Format       *emptyOptions   `arg:"subcommand:format" help:"format and mount a whole disk"`
	Mount        *emptyOptions   `arg:"subcommand:mount" help:"unlock and mount configured disks"`
	Unmount      *emptyOptions   `arg:"subcommand:unmount" help:"unmount and close configured disks"`
	EnsureCopies *previewOptions `arg:"subcommand:ensure-copies" help:"copy missing files across mounted disks"`
	RepairCopies *repairOptions  `arg:"subcommand:repair-copies" help:"find and repair corrupted copies"`
	Lock         *emptyOptions   `arg:"subcommand:lock" help:"make mirror directories read-only"`
	Unlock       *emptyOptions   `arg:"subcommand:unlock" help:"make mirror directories writable"`
}

func (commandLine) Description() string {
	return "Manage encrypted local mirror disks."
}

type callerIdentity struct {
	HomeDir string
	UID     int
	GID     int
}

type platform struct {
	effectiveUID func() int
	effectiveGID func() int
	homeDir      func() (string, error)
	lookupEnv    func(string) (string, bool)
	lookupUserID func(string) (*user.User, error)
}

type handlers struct {
	format       func(context.Context, callerIdentity) error
	mount        func(context.Context, callerIdentity) error
	unmount      func(context.Context, callerIdentity) error
	ensureCopies func(context.Context, bool) error
	repairCopies func(context.Context, bool, bool) error
	lock         func(context.Context) error
	unlock       func(context.Context) error
}

var errDeclined = errors.New("operation declined")

// Run executes mirror with production operating-system integrations and returns
// the process exit code.
func Run(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runWithLogging(ctx, arguments, stdin, stdout, stderr, platform{
		effectiveUID: os.Geteuid,
		effectiveGID: os.Getegid,
		homeDir:      os.UserHomeDir,
		lookupEnv:    os.LookupEnv,
		lookupUserID: user.LookupId,
	}, func(stdout, stderr io.Writer) handlers {
		return commandHandlers(stdin, stdout, stderr, copyDependencies{snapshot: lib.MountedMirrorState, confirm: terminal.Confirm})
	})
}

func runWithLogging(
	ctx context.Context,
	arguments []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	platform platform,
	makeHandlers func(io.Writer, io.Writer) handlers,
) int {
	if stdin == nil || stdout == nil || stderr == nil {
		return 1
	}
	caller, err := loggingCaller(platform)
	var log *commandlog.Log
	if err == nil {
		log, err = commandlog.Open(caller.HomeDir, caller.UID, caller.GID, arguments, stderr)
	}
	if err != nil {
		commandlog.Warn(stderr, fmt.Errorf("command logging unavailable: %w", err))
	} else {
		stdout = log.Writer("stdout", stdout)
		stderr = log.Writer("stderr", stderr)
	}
	code := execute(ctx, arguments, stdin, stdout, stderr, platform, makeHandlers(stdout, stderr))
	if log != nil {
		log.Close(code)
	}
	return code
}

func execute(
	ctx context.Context,
	arguments []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	platform platform,
	handlers handlers,
) int {
	if stdin == nil || stdout == nil || stderr == nil {
		return 1
	}

	var command commandLine
	parser, err := arg.NewParser(arg.Config{Program: "mirror", IgnoreEnv: true}, &command)
	if err != nil {
		fmt.Fprintln(stderr, "fatal:", err)
		return 1
	}
	if err := parser.Parse(arguments); err != nil {
		if errors.Is(err, arg.ErrHelp) {
			parser.WriteHelp(stdout)
			return 0
		}
		fmt.Fprintln(stderr, "error:", err)
		parser.WriteHelp(stderr)
		return 2
	}
	if parser.Subcommand() == nil {
		fmt.Fprintln(stderr, "error: a subcommand is required")
		parser.WriteHelp(stderr)
		return 2
	}

	var runErr error
	switch {
	case command.Format != nil:
		caller, err := requireSudoCaller("format", platform)
		if err != nil {
			runErr = err
		} else if handlers.format == nil {
			runErr = errors.New("format handler is unavailable")
		} else {
			runErr = handlers.format(ctx, caller)
		}
	case command.Mount != nil:
		caller, err := requireSudoCaller("mount", platform)
		if err != nil {
			runErr = err
		} else if handlers.mount == nil {
			runErr = errors.New("mount handler is unavailable")
		} else {
			runErr = handlers.mount(ctx, caller)
		}
	case command.Unmount != nil:
		caller, err := requireSudoCaller("unmount", platform)
		if err != nil {
			runErr = err
		} else if handlers.unmount == nil {
			runErr = errors.New("unmount handler is unavailable")
		} else {
			runErr = handlers.unmount(ctx, caller)
		}
	case command.EnsureCopies != nil:
		if handlers.ensureCopies == nil {
			runErr = errors.New("ensure-copies handler is unavailable")
		} else {
			runErr = handlers.ensureCopies(ctx, command.EnsureCopies.Preview)
		}
	case command.RepairCopies != nil:
		if handlers.repairCopies == nil {
			runErr = errors.New("repair-copies handler is unavailable")
		} else {
			runErr = handlers.repairCopies(ctx, command.RepairCopies.Preview, command.RepairCopies.Resume)
		}
	case command.Lock != nil:
		if handlers.lock == nil {
			runErr = errors.New("lock handler is unavailable")
		} else {
			runErr = handlers.lock(ctx)
		}
	case command.Unlock != nil:
		if handlers.unlock == nil {
			runErr = errors.New("unlock handler is unavailable")
		} else {
			runErr = handlers.unlock(ctx)
		}
	default:
		runErr = errors.New("no command selected")
	}
	if runErr == nil {
		return 0
	}
	if !errors.Is(runErr, errDeclined) {
		fmt.Fprintln(stderr, "fatal:", runErr)
	}
	return 1
}
