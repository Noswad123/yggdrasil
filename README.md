# Yggdrasil

Shared private files and folders with local history for Git worktrees.

## Overview

**Status: initial Go CLI implemented.** Yggdrasil imports explicitly selected,
untracked regular files or directories into an external private store, links them
into worktrees at their normal application paths, and keeps local Git-backed snapshots of their contents.
It does not treat every ignored file as shareable.

**Git retains lifecycle ownership:** use normal Git commands to create, move,
remove, and prune worktrees and manage branches. Optional `ygg hooks install`
enables automatic attachment after normal `git worktree add` and branch
switch/checkout. Installation is opt-in, not automatic; explicit `ygg attach`
remains available for existing worktrees and retries. No worktree lifecycle
wrappers or background history watcher are implemented. Routine edit history
requires manual snapshots (imports and restores also create checkpoints).

The goal is disposable source checkouts without repeated copying of private
configuration. Application startup, authentication, dependency installation, and
runtime selection remain application-owned. Yggdrasil does not start services.

## Requirements and installation

- macOS or Linux; Windows is not supported.
- Go 1.23+.
- Git 2.31+ on `PATH` (discovery uses `--path-format=absolute`).
- Make for the build/install targets below.

From the Yggdrasil source directory:

```sh
make                  # Build bin/ygg and install ~/.local/bin/ygg
ygg help
```

Put `~/.local/bin` on `PATH` to use `ygg`. `make build` only builds `bin/ygg`;
`make install` builds and installs, just like plain `make`. Override the destination
with `make install INSTALL_DIR=/your/preferred/bin`. `make test` runs the tests,
`make fmt` formats Go sources, and `make clean` removes the local binary, not the
installed one. Alternatively, use `go install ./cmd/ygg` with Go's normal install
directory. Building and displaying help do not require source Git metadata.
Other commands require an existing non-bare Git repository. This source workspace
may lack Git metadata: do not run `ygg init` here until a source Git repository
exists. `ygg init` initializes the **private store**, not the source repository.

## API / Interfaces

```text
ygg [-C directory] [--data-root root] init
ygg [-C directory] [--data-root root] track [--dry-run] <repo-relative-path>
ygg [-C directory] [--data-root root] attach [--dry-run]
ygg [-C directory] [--data-root root] status
ygg [-C directory] [--data-root root] snapshot [-m msg]
ygg [-C directory] [--data-root root] history
ygg [-C directory] [--data-root root] restore --snapshot <hex-id> [--dry-run] <path>
ygg [-C directory] [--data-root root] hooks install [--dry-run]
ygg [-C directory] [--data-root root] hooks status
ygg [-C directory] [--data-root root] hooks uninstall [--dry-run]
```

**Global flags precede the command; command flags precede positional paths.**
`-C` defaults to the current directory. Paths are clean and repository-relative,
even when `-C` names a subdirectory. `track` accepts a regular file or directory;
a directory may have an optional trailing slash.

| Command | Implemented behavior |
| --- | --- |
| `init` | Explicitly create the repository's store; validate it on repeat use. Required before other store commands and hook installation, but not hook status/uninstall. |
| `track` | Import one regular file or full directory, preserve the original, and install one shared symlink at the mapping root. An already-correct managed link is a no-op. Use `--dry-run` to preview. |
| `attach` | Preflight all mappings, then attach missing shared links in this worktree. Existing conflicting files, directories (even empty), or links are refused, not merged or overwritten. Use `--dry-run` to preview. |
| `status` | Report attachment health and unsnapshotted shared changes. Exit 1 for unhealthy links or errors; uncaptured changes alone do not make it fail. |
| `snapshot` | Capture all allowlisted shared contents. Unchanged contents reuse the existing snapshot; default message is `Manual snapshot`. |
| `history` | List private snapshot IDs and message subjects, newest first; never file contents. |
| `restore` | Checkpoint current shared contents, replace one exact managed root (file or entire directory subtree) from a private snapshot, and add a restore commit without resetting history. Use `--dry-run` to preview. |
| `hooks install` | Opt in to shared default `post-checkout` attachment using an initialized store and a stable installed binary. Preserves an ordinary existing hook; refuses configured `core.hooksPath`, known hook-manager dispatchers, and symlinked hook paths. Does not attach existing worktrees. |
| `hooks status` | Inspect hook ownership and pinned executable. Warn and exit 1 if a later `core.hooksPath` setting bypasses the installed hook. Works without the private store. |
| `hooks uninstall` | Remove an intact owned wrapper and restore any preserved original hook. Works without the private store; use the same pinned data root. |

`track`, `attach`, and `restore` apply changes by default. Add `--dry-run` for a
read-only preview without changing files or history. Each apply revalidates under
the store lock rather than trusting an earlier preview. A preview is not a
reservation or a guarantee that later writes will succeed. Restore accepts an
unambiguous hexadecimal commit ID or prefix (4–64 characters), not `HEAD`, branch
names, or revision expressions.

`hooks install --dry-run` and `hooks uninstall --dry-run` also perform zero writes.
Use `ygg hooks help` for hook-specific help and [Hook integration](docs/hooks.md)
for compatibility, pinning, and recovery details. Install from a stable binary
(for example, after `make`); installation through `go run` is refused.

## Data Flow

1. Discover the source worktree and canonical common Git directory.
2. Select an external store by hashing the canonical common Git directory's
   location with full SHA-256. Linked worktrees share this identity; separate
   clones do not automatically share. Moving the main common Git directory
   currently requires migration, which is not implemented.
3. On `track`, copy and verify the file or recursive directory tree, create an
   initial import snapshot,
   add a local Git exclude, preserve the original, install an absolute symlink,
   and record the successful mapping.
4. On `attach`, validate all mappings and attach missing links in the
   selected worktree. Correct links remain unchanged. With the optional hook
   installed, branch checkout events invoke the same attachment engine using the
   pinned source identity, canonical data-root parent, and executable path.
5. Edits through those links update the same shared contents automatically. A
   directory uses a single root symlink: future child additions, deletions, and
   renames are shared without registering each child. Applications may need to
   reload; no watcher is needed for sharing, but edits are not automatically
   captured in history.

The default data root is `$XDG_DATA_HOME/yggdrasil/repos`, or
`~/.local/share/yggdrasil/repos` when `XDG_DATA_HOME` is unset. A set
`XDG_DATA_HOME` must be absolute. Use `--data-root /external/location` as an
explicit alternative, consistently across commands. The data root must not
overlap source worktrees or their Git metadata.

```text
<data-root>/<full-common-dir-hash>/       # Root; printed by init/status
├── config.json                         # generated mapping and source identity
├── imports/import-*/<relative-path>     # preserved original file or subtree
├── restore-*/previous                  # retained pre-restore directory root
└── private/
    ├── .git/                           # local private history
    └── files/<relative-path>            # shared canonical file or directory
```

Configuration is machine-generated JSON outside the source tree. New stores and
successful mapping saves use `version: 2`, `source_common_dir`, and
`mappings: [{"path": "...", "kind": "file"}]` (kind is `file` or `directory`).
Version 1 `managed_paths` remain readable as file mappings; read-only operations
do not migrate configuration. Old file snapshots remain restorable. Older binaries
that support only version 1 refuse version 2 stores. Manage mappings through the
CLI, not manual edits. The implementation uses Go's standard library plus `golang.org/x/text`
for Unicode-normalization-safe ownership checks. The private Git repository is
initialized with no remotes or template hooks; Yggdrasil's Git operations disable hooks.

## Example usage

Start with dummy files in an **existing test repository**, not real secrets or
live state. Replace the paths below with your repository and sibling worktree.

```sh
ygg -C /path/to/existing/repo init
printf 'DEMO=value\n' > /path/to/existing/repo/.env.ygg-demo
# Optional preview, then import.
ygg -C /path/to/existing/repo track --dry-run .env.ygg-demo
ygg -C /path/to/existing/repo track .env.ygg-demo

# Optional: enable automatic attachment for future branch checkouts/worktree adds.
# Requires the stable installed ygg binary; does not init, import, or snapshot.
ygg -C /path/to/existing/repo hooks install --dry-run
ygg -C /path/to/existing/repo hooks install
ygg -C /path/to/existing/repo hooks status

# Git owns creation; its post-checkout hook now attaches registered paths.
git -C /path/to/existing/repo worktree add ../feature -b feature
ygg -C /path/to/existing/feature status

# Without the hook, for existing worktrees, or after reconciling an attach warning:
ygg -C /path/to/existing/feature attach --dry-run
ygg -C /path/to/existing/feature attach

# After editing the shared file through either checkout:
ygg -C /path/to/existing/repo snapshot -m 'Demo settings checkpoint'
ygg -C /path/to/existing/repo history

# Replace <hex-id> with an ID from private history, not source Git history.
ygg -C /path/to/existing/repo restore --snapshot <hex-id> --dry-run .env.ygg-demo
ygg -C /path/to/existing/repo restore --snapshot <hex-id> .env.ygg-demo
```

**Restore affects ALL linked worktrees**, not just the checkout selected by `-C`.
Snapshot messages are explicit user labels, printed by `history`: never put
secrets in them. The CLI reports paths and metadata, not private file contents.

### Optional hook workflow for `~/code/monocle`

After `make` installs the stable binary, explicitly initialize the store and
register only reviewed paths with `track` as above. Once those steps are complete:

```sh
ygg -C "$HOME/code/monocle" hooks install
git -C "$HOME/code/monocle" worktree add "$HOME/code/monocle-feature" -b feature
ygg -C "$HOME/code/monocle-feature" status
```

Normal worktree creation now attempts attachment automatically. Use the same
`--data-root` before the command if registration used a custom root. Existing
worktrees still need an initial manual `attach`. These are opt-in instructions,
not an executed Monocle migration or hook configuration change.

### Folder carry-over example

The following is conditional on an **existing, user-created and approved**
`apps/ix-bap-playwright/.local/shared/` directory containing the intended shared
configuration, fixtures, auth, discovery, runtime files, and cache. Tracking this
parent includes every child in sharing and private snapshots; credentials and
mutable writers need explicit scoping and concurrency rules.
This is a proposed Monocle layout, not an implemented migration; see the
[layout and migration plan](docs/local-layout.md) before organizing real data.
Do not track the current mixed Playwright `.local` directory wholesale.

```sh
# Only after application changes, review, and explicit approval of this subtree:
ygg -C /path/to/monocle init
ygg -C /path/to/monocle track --dry-run apps/ix-bap-playwright/.local/shared/
ygg -C /path/to/monocle track apps/ix-bap-playwright/.local/shared/
ygg -C /path/to/monocle-feature attach --dry-run
ygg -C /path/to/monocle-feature attach
```

The sibling worktree must already exist, and its mapping root must be absent or
the correct link. One root link carries future child changes into both worktrees.
Choose this parent only if **all** contents are approved; otherwise track selected
non-overlapping subfolders such as `shared/config` and `shared/fixtures` instead.

## Edge Cases

- The optional hook runs attachment only for branch checkout events (third hook
  argument `1`), not file checkout (`0`) or `git worktree add --no-checkout`.
  `git reset --hard` has no general hook; no dedicated move/remove hook exists.
  Attachment failures warn without turning checkout into failure; an original
  hook's failure status is still returned. Successful checkout files remain when
  attachment fails: inspect `ygg status`, preserve/reconcile conflicting copies,
  then retry explicit `attach` without overwriting conflicts. Post-checkout runs
  too late to prevent Git changing tracked ownership; tracked-path safeguards
  apply to attachment, not to the preceding checkout.
- Hook installation is local to the repository's common Git metadata, is not
  pushed, and changes no source tracked files or Git configuration. Any effective
  `core.hooksPath` setting refuses installation; configuring it later can bypass
  the hook. Ordinary scripts are preserved and chained, but their own path/`$0`
  changes; relative-to-`$0` scripts may need manual integration. Hook-manager
  compatibility is not universal. See [hook safety and recovery](docs/hooks.md).
- Select only intentional shared paths, as individual regular files or folders.
  A directory must be untracked with no tracked descendants. Tracked paths remain
  Git-owned even if ignored. Symlink inputs (including ancestors), traversal, and
  overlapping file/directory mappings are refused. There is no automatic
  consolidation of existing child mappings into a parent mapping.
- Directory subtrees may contain only regular files and directories: no symlinks,
  special files, `.git`, `.gitignore`, or case/Unicode alias-colliding entry names.
  The full subtree is validated before import; attachment preflights every mapping
  before mutation and rechecks during apply, but later I/O failures can still occur.
- `.gitignore` files cannot be managed: Git does not follow symlinked ignore files.
  Paths crossing a nested repository boundary are refused; register that repository
  separately. Filesystem aliases of tracked paths are also protected. Absent
  Unicode-normalization collisions are conservatively refused even on filesystems
  that could keep those spellings distinct.
- Snapshots include only allowlisted roots, recursively, as raw content without
  Git filters or attributes. Empty directories are represented by Git empty trees,
  not `.gitkeep` files. `Dirty` (reported by `status`) detects child deletions and
  empty-directory changes as well as additions, renames, and byte changes.
  Shared copies use `0600` files and `0700` created directories; executable modes
  are not preserved. The original directory backup retains its original subtree
  permissions behind a private `imports/` parent. Sharing is for the same OS owner,
  not a multi-user permission scheme. The source index is never staged or modified.
- Restore accepts only an exact mapping root, not a child inside a managed folder.
  Directory restore destructively replaces the whole live subtree, including
  current/new children absent from the selected snapshot; it does not merge.
  Current shared contents are checkpointed first, and the old root is retained at
  `<store>/restore-*/previous`, outside the live snapshot allowlist, even on success.
  History is appended, never reset. Clients can observe a briefly absent canonical
  root during the rename swap. Stop writers before restoring; retained recovery
  trees are not automatically pruned.
- Ignore rules go in local Git `info/exclude`, not tracked `.gitignore` files.
  An existing `.gitignore` negation can override these rules and cause failure;
  Yggdrasil does not edit repository policy to defeat it.
- Editors that replace a symlink with a regular file can detach a checkout.
  `status` detects replaced links (`conflict`), missing links, tracked paths, and
  missing/unsafe store targets. There is no automated conflict repair; preserve
  differing copies before deciding how to reconcile them.
  A shared directory avoids replacing a leaf symlink when a writer atomically
  renames a child file, but does not make concurrent writes or sharing safe.
- Imports are **not crash-transactional**. After failure, inspect the store's
  `imports/`, private history, canonical files, and `config.json` before retrying.
  Do not blindly retry, overwrite partial imports, or delete the only surviving
  copy. Configuration records an import only after its link was installed.
  Directory preservation requires a rename: moving the original to `imports/`
  across filesystems may fail, leaving the source unchanged and the canonical
  copy/history checkpoint recoverable. There is no recursive-delete fallback.
- Stale or unavailable locks require manual investigation; locks are never broken
  automatically. Do not remove one without establishing that no operation is active.
- Avoid live databases, process receipts, mutable runtime state, and concurrent
  application writes during import, snapshot, or restore. Snapshots read files
  sequentially, not as an atomic multi-file view. Filesystem checks are not a
  defense against a hostile process replacing directories concurrently.
- Git removal of a worktree does not follow links to delete shared targets.
  However, force-removing a checkout can lose detached regular-file contents;
  inspect health and preserve those copies first. No dedicated move/remove hooks
  or lifecycle safety wrapper exists.
- External storage and local history are **not encryption or backup**, nor access
  isolation: applications and agents can read linked files. Protect the store and
  arrange separate backups. Do not share different environments merely because
  their filenames match.

## Direction and optional integrations

Opt-in post-checkout attachment is implemented; see [Hook integration](docs/hooks.md).
The remaining roadmap is attachment reconciliation, a history watcher, improved
conflict recovery, and explicit sharing with additional clones. These remaining
items are not implemented interfaces or commands. Profiles, broad evidence
management, and general development orchestration are not part of the initial CLI.

For Monocle, the [proposed `.local` layout](docs/local-layout.md) puts config,
fixtures, auth, discovery, runtime files, and cache together under `shared/`;
there is no separate `worktree/` category. Do not import today's mixed `.local`
directory wholesale. Preserve run evidence and unresolved submission/reconciliation
records separately. Shared credentials enter private history, mutable writers need
coordination, and shared process receipts do not confer ownership of running
processes. Live sockets/FIFOs and symlinks cannot be imported into a managed folder.
Dependency environments and `node_modules` remain application-managed; selected
regular-file caches are a deliberate sharing choice, not blanket dependency sharing.
This proposal requires separate application work and moves no data here.

Kitsune/tmux may open terminal workspaces; `wtui` or personal wrappers may call
explicit attachment after Git creates a checkout, or users may opt in to the
post-checkout hook. Do not assume it is installed or active; check `ygg hooks status`.
Djinn can expose helpers, Waystone can bookmark locations, and GNU Stow
can distribute non-secret wrappers—not private values. A future 1Password CLI
integration is optional, not a dependency or a new secrets-management layer.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
go test -count=1 -run TestCLIWorktreeLifecycle -v ./cmd/ygg
go test -count=1 -run TestCLIDirectoryWorktreeLifecycle -v ./cmd/ygg
go test -count=1 -run TestHooks -v ./cmd/ygg
```

Tests use temporary repositories and dummy private files, not your real secrets.
The lifecycle tests exercise two worktrees, shared edits, folder additions and
deletions, empty directories, history, restore, and removal without changing the
source index. Case-sensitive-only tests skip on
case-insensitive volumes; Linux validation is still needed before release.

## Related files

- [`docs/hooks.md`](docs/hooks.md): opt-in hook workflow, compatibility limits, and recovery.
- [`cmd/ygg/hooks.go`](cmd/ygg/hooks.go): hook CLI and warning-only automatic attachment handler.
- [`internal/store/hooks.go`](internal/store/hooks.go): wrapper ownership, executable/data-root pinning, installation, and removal.
- [`cmd/ygg/main.go`](cmd/ygg/main.go): CLI parsing, previews, output, and exit status.
- [`internal/store/store.go`](internal/store/store.go): identity, mappings, imports, attachments, private history, restore, and safety checks.
- [`internal/store/directory.go`](internal/store/directory.go): typed roots, recursive inventory, directory import, and staged subtree restore.
- [`internal/store/tree_history.go`](internal/store/tree_history.go): raw Git trees, empty directories, and historical tree validation.
- [`internal/store/directory_test.go`](internal/store/directory_test.go): folder lifecycles, compatibility, safety, previews, and subtree restore.
- [`internal/store/ownership_regression_test.go`](internal/store/ownership_regression_test.go): Unicode-root history and common-worktree ownership regressions.
- [`docs/local-layout.md`](docs/local-layout.md): proposed Monocle private-data organization and explicit carry-over migration plan.
- [`internal/store/store_test.go`](internal/store/store_test.go): shared-worktree lifecycle, permissions, allowlist, and failure coverage.
- [`internal/store/plan_test.go`](internal/store/plan_test.go): read-only previews and apply-time revalidation coverage.
- [`internal/store/safety_test.go`](internal/store/safety_test.go): ignore-file, filesystem-alias, and nested-repository protections.
- [`cmd/ygg/main_test.go`](cmd/ygg/main_test.go): CLI and end-to-end worktree coverage.
