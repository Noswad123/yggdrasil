# Opt-in post-checkout attachment

## Overview

`ygg hooks install` opts one source repository into automatic attachment of
registered private files and folders after branch checkout events. Git still
creates and manages worktrees. Installation requires an initialized private
store and a stable installed `ygg` binary, such as the one installed by `make`;
`go run` installation is refused.

Installation does not initialize a store, import paths, attach existing
worktrees, or create snapshots. Automatic attachment starts no services,
dependency installs, or additional hooks. Sharing is immediate through the
links, but edit history still requires manual snapshots; a history watcher is
roadmap work.

## API / Interfaces

```text
ygg [-C repo] [--data-root root] hooks install [--dry-run]
ygg [-C repo] [--data-root root] hooks status
ygg [-C repo] [--data-root root] hooks uninstall [--dry-run]
```

Global flags precede `hooks`; these subcommands accept no positional arguments.
Both dry-run operations perform zero writes. `hooks status` and `hooks uninstall`
can operate even if the private store is missing, using the same source
repository and pinned data-root identity.

Installation pins:

- The absolute canonical executable path. Upgrading the binary at that same path
  works; moving it requires uninstalling and reinstalling from the new location.
- The canonical **data-root parent**, not the per-repository hash directory
  printed by `init`/`status`. Supply the same `--data-root` used for registration.
- The canonical source common Git directory identity, checked by the handler
  before attachment. Separate clones do not inherit the integration.

The wrapper is installed at `<common-git-dir>/hooks/post-checkout`, shared by the
repository's linked worktrees. Ownership metadata lives beside it in
`.ygg-post-checkout.json`; an ordinary existing hook is preserved as
`post-checkout.ygg-original`. Installation changes neither source tracked files
nor Git configuration, sets no global configuration, and is not pushed.

`hooks status` reports `not-installed`, `installed`, or `bypassed` for an intact
installation. A later effective `core.hooksPath` setting produces `bypassed`, a
warning, and exit status 1. Integrity/configuration errors also fail rather than
silently treating a modified or partial installation as healthy.

## Data Flow

1. Installation validates the existing store, default hook location, ownership,
   and effective Git hook configuration, then publishes the local wrapper and
   manifest. It does not attach the current or other existing worktrees.
2. Git invokes `post-checkout` after normal `git worktree add` and branch
   `git switch`/`git checkout`. The wrapper first runs any preserved executable
   ordinary hook with the same arguments, working directory, and environment.
   A non-executable original remains inactive.
3. When the third hook argument is `1`, the wrapper calls the pinned binary with
   the pinned data-root parent and expected common-directory identity. File
   checkout events (`0`) do not attempt attachment; `git worktree add --no-checkout`
   does not invoke the hook.
4. The handler validates identity and calls the normal `Attach` engine. It locks
   and validates the store, preflights all mappings, updates local excludes, and
   creates missing links. Correct links remain unchanged. It makes no snapshots.
5. Automatic attachment failures warn without making checkout fail. The wrapper
   still returns the preserved original hook's exit status, including failures.

## Edge Cases

### Compatibility and scope

- Any configured effective `core.hooksPath` from global, system, local, worktree,
  or included configuration refuses installation; no configuration is changed.
  Setting it later can bypass the wrapper: check `hooks status` when automatic
  attachment stops working. Configuration injection through `GIT_CONFIG_COUNT`
  or `GIT_CONFIG_PARAMETERS` is refused during hook configuration inspection.
- Known generated hook-manager dispatchers and symlinked hook paths are refused.
  This is not universal hook-manager compatibility; custom integrations require
  manual review rather than replacing their dispatcher.
- An ordinary original script is backed up and chained, but its own filename and
  `$0` change. Scripts finding resources relative to `$0` may require manual
  integration. Uninstall restores an intact preserved original, including its
  mode; an inactive original is not activated by installation.
- There is no general hook for `git reset --hard` and no dedicated worktree
  move/remove hook. Existing worktrees need an initial manual `ygg attach`;
  installation itself does not reconcile them.

### Attachment warnings and retries

Conflicting source files or mapping roots (including empty directories), tracked
paths, a locked/missing/malformed store, wrong repository identity, and a missing
or non-executable pinned binary cause warnings, not Yggdrasil checkout failures.
Original hook failures are still returned. Attachment does not overwrite or
merge conflicts. Later I/O errors can leave partial attachment even after a
successful preflight.

The hook runs **after** checkout: it cannot prevent Git changing tracked ownership
beforehand. Tracked-path safeguards apply to attachment, not to the preceding
Git operation. Ordinary files produced by a successful checkout remain when
attachment fails. Inspect `ygg status` in that worktree, preserve differing copies,
reconcile the conflict deliberately, then run `attach --dry-run` and `attach` to
retry. Do not overwrite conflicts merely to silence the warning.

### Ownership and recovery

Status and uninstall do not require loading the private store. If it disappears,
use the same pinned canonical `--data-root` parent to inspect or remove the
integration; do not pass the per-repository hash root or initialize a replacement
just to uninstall.

Modified wrappers, backups, or ownership manifests are refused rather than
overwritten or discarded. Hook management uses
`<common-git-dir>/.ygg-post-checkout.lock`; it is never automatically broken.
Investigate a stale lock and establish that no operation is active before any
manual recovery. Partial publication/removal failures can leave recoverable
files: inspect the wrapper, manifest, preserved original, and lock before manual
recovery. Do not blindly delete backups or retry over partial state.

## Example usage

For `~/code/monocle`, first run `make` from the Yggdrasil source directory. In the
existing Monocle repository, explicitly initialize the store and register only
reviewed paths with `track`. This example assumes those steps are complete and
uses the default data root; it does not migrate or approve any Monocle data.

```sh
ygg -C "$HOME/code/monocle" hooks install --dry-run
ygg -C "$HOME/code/monocle" hooks install
ygg -C "$HOME/code/monocle" hooks status

# Normal Git worktree creation automatically attempts attachment.
git -C "$HOME/code/monocle" worktree add "$HOME/code/monocle-feature" -b feature
ygg -C "$HOME/code/monocle-feature" status

# Fallback for existing worktrees, no hook, or retries AFTER reconciling conflicts.
ygg -C "$HOME/code/monocle-feature" attach --dry-run
ygg -C "$HOME/code/monocle-feature" attach

# Optional removal; restores any intact preserved original hook.
ygg -C "$HOME/code/monocle" hooks uninstall --dry-run
ygg -C "$HOME/code/monocle" hooks uninstall
```

For a custom root, put `--data-root /external/root` before `hooks`, `status`, or
`attach`, consistently with registration. The installed wrapper passes its
pinned root automatically, independent of later environment defaults. These
commands are instructions only; no real repository hook is installed by this
documentation change.

## Related files

- [README](../README.md): installation, mapping workflow, and general safety limits.
- [`cmd/ygg/hooks.go`](../cmd/ygg/hooks.go): CLI, output, and warning-only handler.
- [`cmd/ygg/main.go`](../cmd/ygg/main.go): global flags and command dispatch.
- [`internal/store/hooks.go`](../internal/store/hooks.go): wrapper generation,
  ownership checks, pinning, and hook management.
- [`internal/store/store.go`](../internal/store/store.go): repository identity,
  store validation, locking, and attachment safeguards.
