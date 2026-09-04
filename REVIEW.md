# Top-to-bottom review

Reviewed 2026-09-04 at `5843e53809d1c262f858ddef06406a656b88734e`.

## Scope and validation

Read all 47 tracked files in full: production code, tests, VM/image automation, shell wrappers, module manifests/checksums, documentation, guidelines, backlog, and license. Inspected the ignored generated `mirror` ELF's file type and Go build metadata; did not disassemble it or use it as evidence that current source works. Git internals and downloaded dependencies were not part of the source review.

- `go run ./bin/check.go`: **PASS**, including tests, race tests, vet, staticcheck, module consistency, formatting, whitespace, and wrapper checks.
- `env MIRROR_E2E_VM= MIRROR_E2E_KEEP= go test -race -count=1 ./...`: **PASS**, uncached.
- Added review-only tests through a Go overlay outside the repository: **13 failing behavioral probes** against actual production functions. These are additional tests exposing gaps, not failures in the existing suite. Storage-command failures were injected; filesystem probes used disposable directories. No destructive tool was given a host block device.
- Did not run the opt-in VM lifecycle test, install software, or test physical power-loss durability. Cancellation and root-loss findings below distinguish simulated OS transitions from actual temporary-filesystem/subprocess observations.

The review and evidence above describe the initial revision, before fixes. Findings are resolved individually below; `[done]` marks verified fixes, and unmarked findings remain open. P1 means a safety or silent-omission issue to address first; P2 means a functional/recovery gap; P3 means maintainability or documentation debt.

## Safety and correctness findings

### 1. [done] P1 — Missing data causes valid sidecar evidence to disappear from consensus

**Location:** `internal/repaircopies/files.go:168-213`; `internal/repaircopies/repaircopies.go:103-128,227-250`.

The scanner skips every regular `.b2sum` entry and reads a sidecar only after finding its data file. The observation model also requires a valid actual-data checksum. Consequently, a mounted mirror with missing data but a valid surviving sidecar contributes no evidence.

This can turn an ambiguous decision into an apparently safe repair. With three configured mirrors:

- A has data X and sidecar X.
- B has data Y and sidecar X.
- C has no data but sidecar Y.

X has support from A/B and Y from B/C. Both exceed half of three, so the documented rule requires refusal. Actual `Scan`/`BuildLog` observes only A/B and selects X for repair. The arithmetic is correct for the observations; the scanner dropped decisive contrary evidence.

**Evidence:** `TestAuditOrphanSidecarMustParticipateInConsensus` failed: two observed copies, X selected, one repair scheduled.

**Recommendation:** Model checksum evidence independently of existing data-copy repair targets. Observe surviving sidecars even when data is absent, retain the configured denominator, and still require a real source and exactly one qualifying candidate. This need not make repair fill missing data copies.

**Resolution (2026-09-04):** Scanner and consensus now retain sidecar-only evidence. Log v3 records absent data explicitly; older logs require a fresh scan because their evidence may be incomplete. Evidence-only entries are neither sources nor repair targets, and resume/completion revalidate both data absence and exact sidecar bytes. The original review probe now passes. Permanent tests in `internal/repaircopies/sidecar_evidence_test.go` cover ambiguity, a necessary sidecar-only vote, malformed evidence, absent-mirror denominators, source requirements, preview, save/load, resume, evidence drift, completion, and old-log rejection. Ensure's scanner consumer is adapted without changing its separate missing-copy recovery policy. `go run ./bin/check.go` passes, including race tests, vet, staticcheck, and module/wrapper checks.

### 2. P1 — Relock cleanup can chmod a different filesystem/directory after root loss

**Location:** `internal/repaircopies/command.go:121-130`; `internal/ensurecopies/ensurecopies.go:49-57`; `lib/lib.go:40-58`.

Cleanup retains root path strings, not the identities/descriptors of the directories it unlocked. `LockRoots` opens those paths afresh. If revalidation detects a disappeared/replaced mount after unlocking, the deferred cleanup can operate on the directory now exposed at the same path. `rootfs.Root.Check` cannot catch this: cleanup establishes a new baseline from the replacement itself.

**Evidence:** `TestAuditCleanupDoesNotChmodReplacementRoot` replaced a temporary root during the post-unlock snapshot callback, then returned a mount-loss error. Cleanup changed the unrelated replacement from `0700` to `0544`; the original directory remained `0744`. This simulates path replacement without mounting or unmounting a host filesystem.

**Recommendation:** Carry verified root identities/owned descriptors through unlock, apply, and cleanup. Refuse to chmod a replacement path; restore only directories belonging to the operation's original roots. Apply the same ownership discipline to ordinary lock/unlock operations.

### 3. P1 — Failed or cancelled storage commands are assumed to have made no changes

**Location:** `internal/mountdisks/run.go:145-204`; `internal/formatdisk/formatdisk.go:223-286`.

The `opened`/`mounted` cleanup flags are set only after the external command returns success. A process can complete its kernel operation and then be killed, lose its response, or exit with an error. In that case the operation has happened but cleanup skips it.

For example, a mount that takes effect before cancellation leaves `current.mounted` false. Rollback skips `umount` and tries to close a still-busy mapper instead. The analogous failed `cryptsetup open` can leave an untracked open mapper. Formatting has the same success-only accounting.

**Evidence:** `TestAuditRollbackReconcilesFailedCommandSideEffects` injected a mount side effect followed by `context.Canceled`. Both mount and decrypted mapper remained active; cleanup attempted zero unmounts.

**Recommendation:** Record attempted transitions and reconcile actual state after errors using an independent bounded context. Verify backing-device and mount identity before cleaning up, so reconciliation does not remove pre-existing or unrelated resources. Add failure-after-side-effect cases, not just failure-before-side-effect cases, to both state-machine suites.

### 4. P1 — A legitimate payload ending in `.b2sum` is silently omitted

**Location:** `internal/repaircopies/files.go:198-200`; `internal/ensurecopies/ensurecopies.go:59-85`; `readme.md:77-79`.

The suffix is treated as an unconditional exclusion, not a validated relationship to another object. A standalone application file named `database.b2sum` is neither copied nor reported as unsupported. Ensure can return success with zero actions, and repair can report a clean scan, while that file has no redundant copy.

This is distinct from finding 1: one loses genuine checksum evidence; this one silently loses coverage of user payloads. The documentation lists supported object types but does not reserve this suffix for all payload names.

**Evidence:** `TestAuditStandaloneChecksumNamedPayloadIsNotSilentlySkipped` completed plan/apply successfully with no actions and no destination file.

**Recommendation:** Define and enforce the payload/metadata namespace at ingest. Either support arbitrary payload names through unambiguous metadata storage, or explicitly reject conflicting/orphan names with a useful explanation. Do not report success after silently excluding them.

### 5. P2 — Ensure preflight does not check the sidecar destinations it will publish

**Location:** `internal/ensurecopies/ensurecopies.go:131-145`; `internal/repaircopies/files.go:449-468`.

Planning checks that a missing data path is absent, but not that its adjacent sidecar is absent. `PublishMissing` checks both later. Thus a known, pre-existing obstruction is discovered only after earlier actions have published data and metadata. Preview advertises a plan that cannot succeed.

A common trigger is deleted data with its correct checksum sidecar still present: repair ignores the orphan, and ensure refuses to restore the missing data because its sidecar already exists.

**Evidence:** `TestAuditSidecarCollisionMustFailPreflight` supplied files `a` and `z`, with only `z.b2sum` on the destination. Planning succeeded; apply published `a` before failing on the already-existing `z.b2sum`.

**Recommendation:** Preflight the complete data/sidecar publication pair for every action. Define an explicit recovery policy for an existing matching sidecar; absent such a policy, reject it before any publication or chmod. Preserve no-replace publication checks as the final guard.

### 6. P2 — Empty directories are never replicated

**Location:** `internal/ensurecopies/ensurecopies.go:59-85,148-182`; `internal/repaircopies/files.go:168-219`.

Both scans recurse through directories but retain only files or symlinks. Directories are created only as parents of published entries. An empty directory, including an entire empty directory tree, produces no action and disappears from the replica's structure without a warning.

**Evidence:** `TestAuditEmptyDirectoryIsReplicated` returned a successful empty plan/apply but did not create the destination directory.

**Recommendation:** Include directory entries in the ensure plan, with type-conflict checks and explicit creation ordering. If directory structure is deliberately out of scope, the command/documentation must stop claiming general directory support and expose the omission.

### 7. P2 — Ordinary relative symlinks unnecessarily block an entire ensure run

**Location:** `internal/ensurecopies/ensurecopies.go:168-175`; `readme.md:77-79`.

`scanLinks` accepts only targets beginning with `../`. Normal links such as `link -> target` and `link -> ./target` fail the whole plan. The publication code already treats targets as opaque strings and never follows them, so this prefix check does not provide the advertised traversal confinement. Conversely, the accepted prefix permits paths escaping the mirror.

**Evidence:** `TestAuditOrdinaryRelativeSymlinkIsSupported` failed on `link -> target` before any copying.

**Recommendation:** Preserve supported symlink targets opaquely and enforce confinement on filesystem access, not an arbitrary target prefix. If external-target restrictions are actually wanted, define that policy explicitly and validate it consistently.

### 8. P2 — One unresolved file discards the completed scan and hides subsequent damage

**Location:** `internal/repaircopies/repaircopies.go:64-101`; `internal/repaircopies/command.go:57-96`.

`BuildLog` returns an empty log on the first consensus failure. The command has already persisted `scanning`, but never saves the completed observations or prints the other damaged paths. `--resume` then rejects the log. A fully completed, potentially multi-terabyte scan must be repeated just to discover the next unresolved file.

Refusing ambiguous repair is correct; throwing away the observations and conflating completed-but-unresolved with interrupted scanning is not necessary for safety.

**Evidence:** `TestAuditUnresolvedScanRetainsAllDamage` scanned two unresolved files. Only `a-unresolved` appeared in the error, `z-unresolved` was absent from the report, and the saved status remained `scanning`.

**Recommendation:** Separate scan completion/observations from repairability. Persist and display all unresolved findings while keeping their application fail-closed. Make any log-schema change an explicit hard cutover.

### 9. P2 — Unmount recovery requires the filesystem to be healthy enough to mount

**Location:** `internal/mountdisks/inventory.go:179-184,242-251`; `internal/mountdisks/run.go:222-244`.

Unmount uses the same preflight policy as mounted-root discovery/mounting. It refuses a correctly identified open mapper unless it reports ext4 and is not read-only. A failed format, damaged filesystem signature, or read-only block device can therefore prevent `mirror unmount` from closing the mapper precisely when recovery is needed.

**Evidence:** `TestAuditUnmountCanCloseKnownNonExt4Mapper` supplied a configured LUKS partition with its expected open, unmounted crypt mapper and an empty filesystem type. Unmount rejected it before `cryptsetup close`.

**Recommendation:** Share inventory parsing, but separate action-specific policies. For unmount/close, validate identity, ownership of the mapping, mount relationships, and unexpected holders; do not require a mountable writable filesystem merely to deactivate it.

### 10. P2 — Formatter discovers missing required tools only after erasing the partition table

**Location:** `internal/formatdisk/formatdisk.go:129-220,263-299`; `internal/formatdisk/system.go:12-19`.

The preflight inventories disks and mountpoints but never checks availability of the tools needed to finish. On a system with `fdisk` but no `cryptsetup`, it performs the destructive partition-table write and only then discovers that encryption cannot be created. The same late discovery applies to later filesystem/mount tools.

**Evidence:** `TestAuditMissingToolDetectedBeforeDiskErase` injected missing `cryptsetup`; the formatter had already issued successful destructive `fdisk` before returning the missing-tool error. No real disk was used.

**Recommendation:** Verify required executables and necessary capabilities before requesting destructive confirmation. Keep the immediate identity/state revalidation afterward. Do not install dependencies automatically as part of formatting.

### 11. P2 — Command cancellation does not own descendants or bound inherited output pipes

**Location:** `internal/system/exec.go:17-45`; related test helper: `tests/e2e/mirror_format_vm_test.go:137-199`.

`exec.CommandContext` kills the direct process, not its descendants. `Exec.Output` captures stdout/stderr through pipes and calls `Run` without a wait bound. A surviving helper that inherited those descriptors can keep `Run` blocked after cancellation indefinitely. With direct file output, a descendant can instead survive the parent's return and continue working after mirror's operation lock is released.

**Evidence:** `TestAuditExecCancellationBoundsInheritedPipes` ran actual helper subprocesses. Cancelling the direct process still took about two seconds to return, until its deliberately bounded child exited and released the pipes. This is not kernel-blocked disk I/O.

**Recommendation:** Own and terminate the command's descendant process group, join cleanup before releasing the operation lock, and bound pipe draining. A pipe wait timeout alone is insufficient if a surviving helper can still mutate storage. The interactive VM test runner likewise should not assume killing only its direct process closes every inherited output descriptor.

### 12. P2 — Configuration reads can block outside the cancellation machinery

**Location:** `internal/mountdisks/inventory.go:66-94,116-121`; `internal/mountdisks/run.go:283-293`.

`loadConfig` uses unrestricted `os.ReadFile`, without context, regular-file validation, or a size bound. A mistaken FIFO at `.mirror/disks` blocks before cancellable inventory even starts. Since the CLI converts SIGINT/SIGTERM into context cancellation, those signals do not unblock this read, and the operation lock remains held. A very large configuration is also read fully into memory before validation.

**Evidence:** `TestAuditConfigInputMustNotBlockOnFIFO` observed the production loader blocked on a disposable FIFO. The test explicitly opened/closed a writer to release it before failing; it left no blocked goroutine.

**Recommendation:** Open configuration nonblocking and without unintended traversal, verify it is a regular file, bound metadata size, and propagate context through reading/parsing. Treat an invalid config type as a preflight error.

### 13. P2 — Failed plan output does not stop confirmation or application

**Location:** `internal/repaircopies/repaircopies.go:130-150`; `internal/repaircopies/command.go:99-113`; `internal/terminal/password.go:170-185`. Similar unchecked prompt writes exist in format/mount.

`PrintRepairs` returns an error type but discards every write error and returns nil. `terminal.Confirm` also ignores failure to display its prompt. The workflow can proceed to confirmation, mutate data, save a completed log, and return success even though the operator never received the plan. A full output destination is a concrete case; unlike some broken-pipe cases, it does not necessarily terminate the process.

**Evidence:** `TestAuditRepairDoesNotProceedWithoutDisplayingPlan` made every report write fail. The confirmation callback was still called and the complete repair returned nil.

**Recommendation:** Propagate report and prompt write failures and abort before confirmation/mutation when the plan could not be delivered. Preserve original operational errors if later cleanup/status reporting also fails.

## Design and maintenance findings

### 14. P3 — Safety-critical inventory schemas and parsing are duplicated

**Location:** `internal/formatdisk/formatdisk.go:23-65,369-403,529-581`; `internal/mountdisks/inventory.go:16-51,286-333`.

Formatting and mounting maintain separate block-device structs, JSON decoding/flattening, mount representations, and validation helpers. They already differ in canonicalization and empty/invalid mount-tree handling. Fixes to inventory edge cases have to be found and applied twice, while tests mostly generate JSON from each package's own duplicated types rather than exercising common real-output fixtures.

**Recommendation:** Share a small, typed inventory/parser boundary and representative JSON fixtures. Keep format/mount/unmount policies separate; a generic storage orchestration framework would add complexity rather than remove it.

### 15. P3 — Unified CLI stream injection stops at the copy/repair handlers

**Location:** `internal/cli/handlers.go:21-48`; `internal/ensurecopies/ensurecopies.go:27-57`; `internal/repaircopies/command.go:25-32`.

Format and mount receive the streams supplied to `cli.Run`; ensure/repair instead write directly to `os.Stdout`, and repair constructs its own terminal prompt. An embedding caller or test cannot capture/control these commands through the advertised CLI boundary. This also leaves the production ensure workflow much less injectable than the other command state machines; its current unit tests exercise `plan`/`apply`, not the full workflow.

**Recommendation:** Pass output and explicit prompt/OS dependencies consistently through command options. Keep the real controlling-terminal policy where required, but make it an explicit boundary instead of silently bypassing caller-provided streams.

### 16. P3 — Image-building automation is in the application command tree

**Location:** `cmd/bake-e2e-image/main.go`; `bin/bake-e2e-image:4`; `bin/check.go:77-81`; `NINA.md`.

The image builder is ordinary standard-library repository automation, but lives under `cmd/` and has its own usage-facing command. This conflicts with the project's explicit placement rule for simple automation (`bin/*.go`) and blurs the otherwise single-application layout. The wrapper checker hardcodes that misplaced path, so it enforces the divergence.

**Recommendation:** Move the builder to a Go automation file under `bin/`, adjust its thin wrapper and checker together, and remove the old command path. VM-only guest helpers can remain under tests.

### 17. P3 — The only backlog item is already implemented

**Location:** `BACKLOG.md:1`; `internal/repaircopies/repaircopies.go:68-80`; `internal/repaircopies/repaircopies_test.go:16-47`.

The backlog says repair sections show disks in random order. `BuildLog` already sorts paths and each repair's copies by root; log validation enforces sorted unique roots, and the existing test verifies sorted output inputs. Keeping the item suggests work remains that has already been completed.

**Recommendation:** Remove the obsolete item or replace it with a specific remaining presentation defect if one is observed. No random-order defect was found in the current repair path.

## Reproduction artifacts

Review-only test sources and the overlay are retained outside the worktree at:

```text
/home/nathants/.nina/runs/home_nathants_repos_mirror_b51aa2df9bd3980b_20260904T225854Z_rfHxi/scratch/
```

Files: `ensure_review_test.go`, `repair_review_test.go`, `mount_review_test.go`, `system_review_test.go`, `format_review_test.go`, and `review-overlay.json`.

From this repository, replay the original review probes without changing source files (resolved findings should now pass):

```bash
go test -count=1 \
  -overlay /home/nathants/.nina/runs/home_nathants_repos_mirror_b51aa2df9bd3980b_20260904T225854Z_rfHxi/scratch/review-overlay.json \
  -run TestAudit -v \
  ./internal/ensurecopies ./internal/repaircopies ./internal/mountdisks \
  ./internal/system ./internal/formatdisk
```

At the initial reviewed revision, the helper subprocess test passed and the 13 behavioral assertions failed. That output is retained in the same run's `shell/f327edfbb2e7b8b0ba48e6487e0a52f8/stdout`. These probes assert the proposed safe/complete behavior; they do not replace production logic or weaken the existing tests.
