# Proposed Monocle Playwright `.local` layout

## Overview

**Proposed only. No application source or private data is changed or moved by
this document.** The Playwright app is present on a Monocle feature branch, not
on main. This plan requires a separate Monocle tracked code/docs change before
adoption; Yggdrasil does not implement application migration or orchestration.

The supplied metadata-only inventory of `apps/ix-bap-playwright/.local` in that
feature worktree reports roughly 700 regular files: configuration and fixtures,
candidate/toolkit run directories, warehouse/discovery output,
publication/drafts, and mixed root-level JSON, PNG, and archive files. This is
not a content audit or permission to share them. Private values and unique run
names are intentionally omitted.

The goal is one explicit carry-over boundary for shared development context:
configuration, fixtures, authentication, discovery, runtime files, and caches.
Submission guards and evidence stay separate from that boundary. This does not
mean importing the current mixed `.local` tree without first organizing it.

## API / Interfaces

All paths below are **proposed application-owned roots** under
`apps/ix-bap-playwright/.local/`, not existing configuration interfaces.

```text
.local/
├── shared/
│   ├── config/
│   ├── fixtures/
│   ├── auth/
│   ├── discovery/
│   ├── runtime/
│   └── cache/
├── state/
│   └── submissions/
└── evidence/
    ├── runs/
    ├── reviews/
    └── publication/
```

There is no separate `worktree/` directory in this proposal. Its previously
proposed contents now live directly under `shared/` and are intended for
cross-worktree carry-over.

| Proposed root | Intended ownership and retention |
| --- | --- |
| `shared/config/` | Explicitly reviewed and approved stable configuration inputs only. |
| `shared/fixtures/` | Explicitly reviewed and approved stable fixtures; preserve raw bytes and digests. |
| `shared/auth/` | Shared authentication/session files, scoped to their actual origin, environment, and account; private-history retention must be understood. |
| `shared/discovery/` | Shared mutable discovery output; producers need compatible scopes, locking, and conflict handling. |
| `shared/runtime/` | Shared regular runtime files and namespaced process receipts; visibility never transfers process ownership. |
| `shared/cache/` | Shared rebuildable cache; writers and cleanup must respect other active worktrees. |
| `state/submissions/` | Durable unresolved submission/reconciliation guards; **never disposable worktree cleanup**. |
| `evidence/runs/`, `evidence/reviews/`, `evidence/publication/` | Active evidence remains mutable and is not live-shared by default. Archive completed evidence under unique IDs to separately managed, verified external storage. |

The spelling `state/submissions/` alone does not make state durable. Existing
pending submission guards must outlive worktrees. A later application change
must specify explicit durable storage and handoff/recovery before any worktree
can be discarded; this proposal does not solve that automatically. Evidence
retention likewise requires a separate archive policy, not just a local folder.

Future application interfaces should support configurable roots and backward
compatibility for explicitly supplied paths. Yggdrasil supplies only explicit
file/folder mappings, attachment, and local snapshots; no broad orchestrator
behavior belongs here.

Tracking `shared/` means **all six categories and future children are shared and
included in snapshots**. The current implementation does not support excluding
selected children from a folder mapping or giving them separate history policies.
Use selected non-overlapping child mappings instead if a narrower boundary is
needed; do not map the parent as well.

## Data Flow

### Separate Monocle migration work

1. Inventory and categorize producers, consumers, retention needs, and existing
   mappings. Review both stable inputs and mutable state for sharing; do not
   infer environment compatibility from an ignored path or a filename. Define
   origin/account scoping and concurrent-writer rules before moving mutable data.
2. Record and preserve raw fixture bytes/digests, evidence paths and references,
   and unresolved submission guards. Decide durable guard storage/handoff and
   completed-evidence archival explicitly, without treating either as cache.
3. Stop all relevant writers across worktrees and launchers. Take an external
   backup and verify it before migration; Yggdrasil history is not that backup.
4. In a separate tracked Monocle code/docs change, update producers and consumers
   for configurable roots, retaining compatibility for explicit existing paths.
   Plan safe, non-overwriting path migration with conflict reporting; do not
   silently merge, replace, or discard existing state.
5. Validate with dummy fixtures and offline tests only: no services and no
   write-capable application tests. Check byte/digest preservation, legacy explicit
   paths, evidence references, guard survival/handoff, migration conflicts, scoped
   authentication, concurrent writers, and runtime ownership validation.
6. After review and explicit approval, create/populate only the intended shared
   subtree through the approved application migration. Preview `ygg track` with
   `--dry-run` first, then intentionally import and attach the chosen roots.

Existing file or directory mappings overlapping a proposed parent need an
explicit future mapping migration. Yggdrasil refuses overlap and does not
consolidate children. Do not work around this by editing generated configuration
or deleting private backups. Preserve existing copies and history while the
migration design is reviewed.

## Edge Cases

- Authentication/session material can remain in Git history after live deletion
  or restore. Tracking `shared/` explicitly includes `shared/auth/` in that history;
  review retention, credential rotation, and external-backup exposure. Reusing a
  session is valid only for its intended origin, account, and environment.
- Machine launcher settings may target the same server checkout from multiple
  local worktrees. An independent source branch does **not** imply an isolated
  external environment, account, service, or write target.
- Copied process receipts are metadata, not ownership of running processes.
  Sharing them does not authorize stopping, reusing, or controlling those processes.
  Namespace receipts by process/session/worktree identity and validate ownership
  before acting. Yggdrasil supports regular files and directories, not live sockets,
  FIFOs, or symlinks; those need an application-owned location outside the managed
  subtree rather than being imported under `shared/runtime/`.
- Discovery writers using atomic rename can replace a leaf symlink. A directory
  mapping keeps child renames inside the shared root, avoiding that leaf-link
  problem, but does not establish that concurrent writers are safe. Shared discovery
  needs compatible destination scopes and locking. Shared cache cleanup likewise
  must not invalidate another worktree's active operation.
- Do not live-share active evidence by default. Completed unique-ID archives need
  external retention and verification; unresolved submission guards are durable
  state, not completed evidence and never cleanup candidates.
- A shared folder includes future additions, deletions, and renames. Review the
  ongoing write policy, not merely today's contents. Folder restore replaces the
  entire subtree for every attached worktree; stop writers and preserve recovery
  data before using it.

## Example usage

**Conditional example only:** these commands assume a user-created, existing,
reviewed `shared/` tree after the separate migration, with compatible regular-file
contents and the credential/history and concurrency implications accepted.
They do not create the layout, migrate current private data, or approve sharing.
Use an existing Monocle feature worktree with the app and an initialized Yggdrasil
store; substitute your repository location.

```sh
ygg -C /path/to/monocle-feature track --dry-run apps/ix-bap-playwright/.local/shared/
# Only after reviewing and approving the complete shared subtree:
ygg -C /path/to/monocle-feature track apps/ix-bap-playwright/.local/shared/
```

Do not also track children of this mapping, or the entire `.local/` directory.
Existing worktrees need an initial attachment (`attach --dry-run`, then `attach`).
The optional [post-checkout integration](hooks.md) can attach registered paths
after future normal Git worktree creation and branch checkouts. Conflicting roots,
including empty directories, are refused. Commands apply by default; `--dry-run`
is a preview, not a migration or reservation.

## Related files

- [Yggdrasil README](../README.md): current folder mapping interfaces and safety limits.
- [Directory engine](../internal/store/directory.go) and
  [tree history](../internal/store/tree_history.go): recursive sharing and restore.

Future Monocle review targets, relative to `apps/ix-bap-playwright/` on the feature
branch (references for follow-up, not files changed or audited here):

- `scripts/launcher-settings.cjs`
- `src/toolkit-fixtures.ts`
- `src/config.ts`
- `src/cli.ts`
- `scripts/discover-blast.cjs`
- `scripts/warehouse-fixtures.py`
- `tests/apps/benchwork/submission.spec.ts`
- `config/playwright/candidate.config.ts`
