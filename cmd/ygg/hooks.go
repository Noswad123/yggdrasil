package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"yggdrasil/internal/store"
)

const hooksUsage = `Usage: ygg [-C directory] [--data-root directory] hooks <command> [options]

  install [--dry-run]    Install shared post-checkout integration for this repository
  status                Show installation state and pinned executable
  uninstall [--dry-run]  Remove integration and restore any preserved original hook

Use a stable installed binary (not go run) for installation. Existing ordinary
hooks are preserved; custom core.hooksPath and hook-manager dispatchers require
manual integration. Git worktree add --no-checkout does not trigger the hook.
`

func hookManager(dir, dataRoot string) (*store.Manager, error) {
	if dataRoot == "" {
		var err error
		dataRoot, err = defaultDataRoot()
		if err != nil {
			return nil, err
		}
	}
	return store.New(dir, dataRoot)
}

func runHooks(args []string, dir, dataRoot string, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "ygg: %v\n", err)
		return 1
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, hooksUsage)
		return 0
	}
	action := args[0]
	if action != "install" && action != "status" && action != "uninstall" {
		return fail(fmt.Errorf("unknown hooks command %q; run ygg hooks help", action))
	}
	f := flags("ygg hooks "+action, stderr)
	var dryRun bool
	if action != "status" {
		f.BoolVar(&dryRun, "dry-run", false, "preview hook changes without writing")
	}
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if f.NArg() != 0 {
		return fail(fmt.Errorf("hooks %s does not accept positional arguments", action))
	}
	m, err := hookManager(dir, dataRoot)
	if err != nil {
		return fail(err)
	}
	var status store.HookStatus
	switch action {
	case "install":
		// Store validation is done by InstallHooks; never initialize implicitly.
		if _, err := os.Lstat(filepath.Join(m.Root(), "config.json")); os.IsNotExist(err) {
			return fail(errors.New("private store is not initialized; run ygg init first"))
		}
		executable, err := os.Executable()
		if err != nil {
			return fail(err)
		}
		status, err = m.InstallHooks(executable, dryRun)
		if err != nil {
			return fail(err)
		}
		if dryRun {
			fmt.Fprintln(stdout, "Preview only (--dry-run). Would ensure post-checkout integration is installed.")
		} else {
			fmt.Fprintln(stdout, "Post-checkout integration installed. Normal Git worktree creation will attach registered paths.")
		}
	case "uninstall":
		status, err = m.UninstallHooks(dryRun)
		if err != nil {
			return fail(err)
		}
		if dryRun {
			fmt.Fprintln(stdout, "Preview only (--dry-run). Would remove Yggdrasil integration and restore any preserved original hook.")
		} else {
			fmt.Fprintln(stdout, "Yggdrasil integration removed or already absent. Any preserved original hook was restored.")
		}
	case "status":
		status, err = m.HooksStatus()
		if err != nil {
			return fail(err)
		}
	}
	fmt.Fprintf(stdout, "Hook: %s\nState: %s\n", status.Path, status.State)
	if status.Executable != "" {
		fmt.Fprintf(stdout, "Pinned executable: %s\n", status.Executable)
	}
	if status.Original {
		fmt.Fprintln(stdout, "An original post-checkout hook is preserved.")
	}
	if status.State == "bypassed" {
		fmt.Fprintln(stderr, "ygg: warning: core.hooksPath bypasses this integration; automatic attachment is inactive here")
		return 1
	}
	return 0
}

// runPostCheckout is the narrow, silent-on-success entry point used by the
// generated hook. It never turns Yggdrasil attachment errors into Git failures.
func runPostCheckout(args []string, dir, dataRoot string, stderr io.Writer) int {
	warn := func(err error) int {
		fmt.Fprintf(stderr, "ygg: warning: automatic attachment failed: %v; run ygg attach in this worktree to inspect\n", err)
		return 0
	}
	f := flags("ygg hook-post-checkout", io.Discard)
	common := f.String("common-dir", "", "expected source repository identity")
	if err := f.Parse(args); err != nil || f.NArg() != 3 || (f.Arg(2) != "0" && f.Arg(2) != "1") {
		return warn(errors.New("invalid post-checkout arguments"))
	}
	if f.Arg(2) == "0" {
		return 0
	}
	m, err := hookManager(dir, dataRoot)
	if err != nil {
		return warn(err)
	}
	if err = m.AttachFromHook(*common); err != nil {
		return warn(err)
	}
	return 0
}
