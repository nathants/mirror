# Project guidelines

- This is Linux-only storage software. Favor explicit, fail-closed state machines over cleverness.
- The only user-facing program is the compiled `cmd/mirror` binary; add functionality as a `mirror` subcommand rather than another entrypoint. Keep shared behavior in normal packages.
- Shell is allowed only for thin `bin/` wrappers that locate the repository and invoke one Go command or the compiled `mirror` binary. Do not put branching, parsing, testing, storage logic, or command pipelines in shell.
- Write simple repository automation as `bin/*.go`, invoke it with `go run`, and use only the Go standard library in those automation programs.
- Invoke external programs with exact argument vectors through `os/exec`; never route program logic through a shell. Prefer structured machine-readable output over parsing presentation text.
- Storage mutations require a complete preflight, an explicit confirmation before the first destructive action, and immediate state/identity revalidation. Production formatting remains restricted to whole `/dev/sd[a-z]+` disks.
- `mirror format`, `mirror mount`, and `mirror unmount` must be launched through sudo and must never invoke sudo internally. Validate sudo's caller metadata against passwd data and use that caller's home directory and ownership.
- Runtime state lives under `~/.mirror/`: `disks` is the configured whole-disk PTUUID list and `repair-last.json` is the atomic resumable repair log. Resume paths must revalidate mounts and listed file state rather than trusting stale log data.
- Tests must never expose host block devices to destructive tools. Real-tool storage tests use disposable image files attached only inside an isolated VM; fast tests inject the operating-system boundary.
- Tests exercise production code rather than reimplementing it. Run `go run ./bin/check.go` after code changes; it enforces formatting, `go test ./...`, race tests, vet, staticcheck, module consistency, diff whitespace, and thin-wrapper syntax.
- Make hard cutovers. Remove superseded entrypoints and compatibility paths rather than retaining parallel implementations.
