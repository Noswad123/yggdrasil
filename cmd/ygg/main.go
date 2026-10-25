package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"yggdrasil/internal/store"
)

const usage = `Yggdrasil — shared private files with local history

Usage:
  ygg [-C directory] [--data-root directory] <command> [options]

Commands:
  init                              Initialize this repository's private store
  track [--dry-run] <path>           Import one repository-relative file or folder
  attach [--dry-run]                 Attach managed files and folders to this worktree
  status                            Show links and unsnapshotted changes
  snapshot [-m message]              Capture the shared private contents
  history                           List snapshot IDs and messages, newest first
  restore --snapshot <id> [--dry-run] <path>
                                    Restore one managed file or folder for ALL worktrees
  hooks install [--dry-run]          Enable automatic post-checkout attachment
  hooks status                      Inspect this repository's hook integration
  hooks uninstall [--dry-run]        Remove integration and restore any original hook

Global flags go before the command; command flags go before positional paths.
Track, attach, and restore apply changes by default; use --dry-run to preview
without changes. Hook installation is opt-in. Contents are never printed.
Background history is not implemented yet.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func defaultDataRoot() (string, error) {
	if root := os.Getenv("XDG_DATA_HOME"); root != "" {
		if !filepath.IsAbs(root) {
			return "", errors.New("XDG_DATA_HOME must be an absolute path")
		}
		return filepath.Join(root, "yggdrasil", "repos"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "yggdrasil", "repos"), nil
}

func flags(name string, stderr io.Writer) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(stderr)
	return f
}

func run(args []string, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "ygg: %v\n", err)
		return 1
	}
	global := flags("ygg", stderr)
	dir := global.String("C", ".", "source repository directory")
	dataRoot := global.String("data-root", "", "external private storage root")
	global.Usage = func() { fmt.Fprint(stderr, usage) }
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if global.NArg() == 0 || global.Arg(0) == "help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	command, commandArgs := global.Arg(0), global.Args()[1:]
	if command == "hooks" {
		return runHooks(commandArgs, *dir, *dataRoot, stdout, stderr)
	}
	if command == "hook-post-checkout" {
		return runPostCheckout(commandArgs, *dir, *dataRoot, stderr)
	}
	var dryRun bool
	var message, revision string
	commandFlags := flags("ygg "+command, stderr)
	switch command {
	case "track", "attach", "restore":
		commandFlags.BoolVar(&dryRun, "dry-run", false, "preview without changing files or history")
		if command == "restore" {
			commandFlags.StringVar(&revision, "snapshot", "", "snapshot commit ID or prefix")
		}
	case "snapshot":
		commandFlags.StringVar(&message, "m", "Manual snapshot", "snapshot message (do not include secrets)")
	case "init", "status", "history":
	default:
		return fail(fmt.Errorf("unknown command %q; run ygg help", command))
	}
	if err := commandFlags.Parse(commandArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	wantArgs := 0
	if command == "track" || command == "restore" {
		wantArgs = 1
	}
	if commandFlags.NArg() != wantArgs {
		return fail(fmt.Errorf("%s expects %d positional argument(s); put flags before paths", command, wantArgs))
	}
	if command == "restore" && revision == "" {
		return fail(errors.New("restore requires --snapshot <commit-id>"))
	}
	if *dataRoot == "" {
		var err error
		*dataRoot, err = defaultDataRoot()
		if err != nil {
			return fail(err)
		}
	}
	m, err := store.New(*dir, *dataRoot)
	if err != nil {
		return fail(err)
	}
	if command == "init" {
		if err = m.Init(); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "Private store: %s\nNo source files registered yet unless previously imported.\n", m.Root())
		return 0
	}
	if _, err = os.Lstat(filepath.Join(m.Root(), "config.json")); os.IsNotExist(err) {
		return fail(errors.New("private store is not initialized; run ygg init first"))
	}
	switch command {
	case "track":
		p := commandFlags.Arg(0)
		plan, err := m.PlanTrack(p)
		if err != nil {
			return fail(err)
		}
		if plan.AlreadyManaged {
			fmt.Fprintf(stdout, "Already linked: %s\n", p)
			return 0
		}
		fmt.Fprintf(stdout, "Import: %s\nShared target: %s\nPreserve original under: %s\nCreate a history checkpoint and replace the source with a shared link.\n", plan.Source, plan.Target, filepath.Join(m.Root(), "imports"))
		if plan.Kind == "directory" {
			fmt.Fprintln(stdout, "Entire folder is shared, including future additions, renames, and deletions.")
		}
		if dryRun {
			fmt.Fprintln(stdout, "Preview only (--dry-run). Omit --dry-run to import.")
			return 0
		}
		if err = m.Track(p); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "Imported and linked: %s\nUse attach in other worktrees to link this file there.\n", p)
	case "attach":
		statuses, err := m.PlanAttach()
		if err != nil {
			return fail(err)
		}
		printStatus(stdout, statuses)
		if dryRun {
			fmt.Fprintln(stdout, "Preview only (--dry-run). Omit --dry-run to attach missing shared links.")
			return 0
		}
		if err = m.Attach(); err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, "Shared files and folders attached. Edits affect every linked worktree.")
	case "status":
		statuses, err := m.Status()
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "Worktree: %s\nPrivate store: %s\n", m.Worktree(), m.Root())
		printStatus(stdout, statuses)
		dirty, err := m.Dirty()
		if err != nil {
			return fail(err)
		}
		if dirty {
			fmt.Fprintln(stdout, "History: uncaptured changes; run ygg snapshot.")
		} else {
			fmt.Fprintln(stdout, "History: no uncaptured changes.")
		}
		for _, s := range statuses {
			if s.State != "linked" {
				return 1
			}
		}
	case "snapshot":
		id, err := m.Snapshot(message)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "Snapshot: %s\nUnchanged contents reuse the existing snapshot.\n", id)
	case "history":
		entries, err := m.History()
		if err != nil {
			return fail(err)
		}
		if len(entries) == 0 {
			fmt.Fprintln(stdout, "No snapshots yet.")
		}
		for _, e := range entries {
			fmt.Fprintf(stdout, "%s  %s\n", e.ID, strings.ReplaceAll(e.Message, "\n", " "))
		}
	case "restore":
		p := commandFlags.Arg(0)
		if err = m.PlanRestore(p, revision); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "Restore: %s\nFrom snapshot: %s\nWARNING: changes the shared path for ALL linked worktrees.\nFor folders, the entire subtree is restored and newer children are removed.\nCurrent shared contents will be checkpointed before replacement.\n", p, revision)
		if dryRun {
			fmt.Fprintln(stdout, "Preview only (--dry-run). Omit --dry-run to restore.")
			return 0
		}
		id, err := m.Restore(p, revision)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "Restored. New snapshot: %s\n", id)
	}
	return 0
}

func printStatus(w io.Writer, statuses []store.FileStatus) {
	if len(statuses) == 0 {
		fmt.Fprintln(w, "No managed paths. Use ygg track <path> to import a private file or folder.")
		return
	}
	t := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, s := range statuses {
		fmt.Fprintf(t, "%s\t%s\n", s.State, s.Path)
	}
	_ = t.Flush()
}
