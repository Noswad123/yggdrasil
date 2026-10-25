package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"yggdrasil/internal/store"
)

func hookEnvironment() []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(e), "GIT_") {
			env = append(env, e)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@localhost",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@localhost")
}

func runExecutable(t *testing.T, executable, dir string, want int, args ...string) string {
	t.Helper()
	cmd := exec.Command(executable, args...)
	cmd.Dir = dir
	cmd.Env = hookEnvironment()
	b, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	if code != want {
		t.Fatalf("%s %v: exit %d, want %d: %s", executable, args, code, want, b)
	}
	return string(b)
}

func buildHookCLI(t *testing.T, base string) string {
	t.Helper()
	dir := filepath.Join(base, "stable binary's folder")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "ygg")
	cmd := exec.Command("go", "build", "-o", binary, ".")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build hook CLI: %v: %s", err, b)
	}
	return binary
}

func TestHooksCLIHelpAndArguments(t *testing.T) {
	for _, args := range [][]string{
		{"hooks"}, {"hooks", "help"}, {"hooks", "--help"},
		{"hooks", "install", "--help"}, {"hooks", "status", "--help"}, {"hooks", "uninstall", "--help"},
	} {
		invoke(t, 0, args...)
	}
	invoke(t, 1, "hooks", "unknown")
	invoke(t, 1, "hooks", "install", "extra")
	invoke(t, 2, "hooks", "status", "--dry-run")
	invoke(t, 2, "hooks", "install", "--yes")
	// The internal handler never makes Git fail or needs a repository for flag 0.
	invoke(t, 0, "hook-post-checkout", "old", "new", "0")
	if out := invoke(t, 0, "hook-post-checkout", "invalid"); !strings.Contains(out, "warning") {
		t.Fatal("malformed automatic invocation should warn, not fail")
	}
}

func TestHooksRealGitWorktreeLifecycle(t *testing.T) {
	base, repo, _ := cliFixture(t)
	data := filepath.Join(base, "private store's root")
	binary := buildHookCLI(t, base)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	var transcript strings.Builder
	call := func(worktree string, code int, args ...string) string {
		t.Helper()
		out := runExecutable(t, binary, repo, code, append([]string{"-C", worktree, "--data-root", data}, args...)...)
		transcript.WriteString(out)
		return out
	}
	gitCall := func(worktree string, code int, args ...string) string {
		t.Helper()
		out := runExecutable(t, git, worktree, code, args...)
		transcript.WriteString(out)
		return out
	}
	const private = "dummy-private-hook-fixture"
	const root = "local/shared"
	if err = os.MkdirAll(filepath.Join(repo, root), 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, root, "settings"), private)
	if out := call(repo, 1, "hooks", "install"); !strings.Contains(out, "run ygg init first") {
		t.Fatal("install should require an initialized store")
	}
	call(repo, 0, "hooks", "status")
	call(repo, 0, "hooks", "uninstall")
	call(repo, 0, "init")
	call(repo, 0, "track", root)
	initialHistory := call(repo, 0, "history")
	m, err := store.New(repo, data)
	if err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	if err = os.MkdirAll(filepath.Dir(hook), 0700); err != nil {
		t.Fatal(err)
	}
	original := "#!/bin/sh\nprintf '%s\\n' \"$3\" >> \"$ORIGINAL_HOOK_LOG\"\nexit 0\n"
	writeFile(t, hook, original)
	if err = os.Chmod(hook, 0751); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(base, "original hook log")
	t.Setenv("ORIGINAL_HOOK_LOG", log)
	config, err := os.ReadFile(filepath.Join(repo, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	call(repo, 0, "hooks", "install", "--dry-run")
	assertContents(t, hook, original)
	if _, err = os.Lstat(hook + ".ygg-original"); !os.IsNotExist(err) {
		t.Fatal("install preview created a backup")
	}
	call(repo, 0, "hooks", "install")
	call(repo, 0, "hooks", "install")
	if out := call(repo, 0, "hooks", "status"); !strings.Contains(out, "State: installed") {
		t.Fatal("missing installation status")
	}
	assertContents(t, filepath.Join(repo, ".git", "config"), string(config))
	assertContents(t, hook+".ygg-original", original)
	// Neither the executable nor data root should be discovered from PATH/HOME.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "different XDG root"))
	t.Setenv("PATH", filepath.Dir(git))
	linked := filepath.Join(base, "new worktree's folder")
	gitCall(repo, 0, "worktree", "add", "-b", "hooked", linked)
	assertContents(t, filepath.Join(linked, root, "settings"), private)
	assertContents(t, log, "1\n")
	call(linked, 0, "hooks", "status")
	if out := call(linked, 0, "history"); out != initialHistory {
		t.Fatal("automatic attachment created a snapshot")
	}
	if out := gitCall(linked, 0, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Fatal("automatic attachment dirtied source Git")
	}
	// File-only checkout still runs the original but must not reattach.
	if err = os.Remove(filepath.Join(linked, root)); err != nil {
		t.Fatal(err)
	}
	gitCall(linked, 0, "checkout", "--", "public")
	if _, err = os.Lstat(filepath.Join(linked, root)); !os.IsNotExist(err) {
		t.Fatal("file checkout invoked automatic attachment")
	}
	assertContents(t, log, "1\n0\n")
	gitCall(linked, 0, "switch", "-c", "branch-checkout")
	assertContents(t, filepath.Join(linked, root, "settings"), private)
	assertContents(t, log, "1\n0\n1\n")
	// A conflicting local path is retained, and Git still succeeds.
	if err = os.Remove(filepath.Join(linked, root)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(linked, root), "local conflict fixture")
	if out := gitCall(linked, 0, "switch", "-c", "conflicting-context"); !strings.Contains(out, "warning: automatic attachment failed") {
		t.Fatal("conflicting auto attachment did not warn")
	}
	assertContents(t, filepath.Join(linked, root), "local conflict fixture")
	if err = os.Remove(filepath.Join(linked, root)); err != nil {
		t.Fatal(err)
	}
	call(linked, 0, "attach")
	writeFile(t, filepath.Join(m.Root(), "lock"), "held")
	if out := gitCall(linked, 0, "switch", "-c", "locked-context"); !strings.Contains(out, "warning: automatic attachment failed") {
		t.Fatal("locked auto attachment did not warn")
	}
	if err = os.Remove(filepath.Join(m.Root(), "lock")); err != nil {
		t.Fatal(err)
	}
	noCheckout := filepath.Join(base, "no checkout")
	gitCall(repo, 0, "worktree", "add", "--no-checkout", "-b", "no-checkout", noCheckout)
	if _, err = os.Lstat(filepath.Join(noCheckout, root)); !os.IsNotExist(err) {
		t.Fatal("--no-checkout unexpectedly attached context")
	}
	wrongCommon := filepath.Join(base, "unrelated Git metadata")
	if err = os.Mkdir(wrongCommon, 0700); err != nil {
		t.Fatal(err)
	}
	beforeConfig, err := os.ReadFile(filepath.Join(m.Root(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if out := call(noCheckout, 0, "hook-post-checkout", "--common-dir", wrongCommon, "old", "new", "1"); !strings.Contains(out, "identity mismatch") {
		t.Fatal("wrong-repository automatic invocation did not warn")
	}
	if _, err = os.Lstat(filepath.Join(noCheckout, "local")); !os.IsNotExist(err) {
		t.Fatal("wrong-repository automatic invocation attached paths")
	}
	assertContents(t, filepath.Join(m.Root(), "config.json"), string(beforeConfig))
	if out := call(repo, 0, "history"); out != initialHistory {
		t.Fatal("wrong-repository automatic invocation changed history")
	}
	// Missing executable is a warning only. Restore it to exercise uninstall.
	if err = os.Rename(binary, binary+".moved"); err != nil {
		t.Fatal(err)
	}
	if out := gitCall(linked, 0, "switch", "-c", "missing-executable"); !strings.Contains(out, "pinned hook executable is missing") {
		t.Fatal("missing hook executable did not warn")
	}
	if err = os.Rename(binary+".moved", binary); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(m.Root()); err != nil {
		t.Fatal(err)
	}
	if out := gitCall(linked, 0, "switch", "-c", "missing-store"); !strings.Contains(out, "warning: automatic attachment failed") {
		t.Fatal("missing store did not warn")
	}
	if _, err = os.Lstat(m.Root()); !os.IsNotExist(err) {
		t.Fatal("hook reinitialized a missing store")
	}
	call(linked, 0, "hooks", "uninstall", "--dry-run")
	call(linked, 0, "hooks", "status")
	call(linked, 0, "hooks", "uninstall")
	call(linked, 0, "hooks", "uninstall")
	assertContents(t, hook, original)
	if st, err := os.Stat(hook); err != nil || st.Mode().Perm() != 0751 {
		t.Fatal("uninstall did not restore original mode")
	}
	assertContents(t, filepath.Join(repo, ".git", "config"), string(config))
	if strings.Contains(transcript.String(), private) {
		t.Fatal("hook output exposed private contents")
	}
}

func TestHooksPreserveOriginalFailureThroughGit(t *testing.T) {
	base, repo, data := cliFixture(t)
	binary := buildHookCLI(t, base)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	call := func(args ...string) {
		t.Helper()
		runExecutable(t, binary, repo, 0, append([]string{"-C", repo, "--data-root", data}, args...)...)
	}
	call("init")
	writeFile(t, filepath.Join(repo, "private"), "private fixture")
	call("track", "private")
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	if err = os.MkdirAll(filepath.Dir(hook), 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, hook, "#!/bin/sh\nexit 7\n")
	if err = os.Chmod(hook, 0755); err != nil {
		t.Fatal(err)
	}
	// Git versions may normalize a hook's nonzero exit code. Compare with the
	// original hook directly rather than assuming Git itself will return 7.
	baseline := exec.Command(git, "switch", "-c", "baseline-hook-failure")
	baseline.Dir = repo
	baseline.Env = hookEnvironment()
	output, err := baseline.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() == 0 {
		t.Fatalf("expected original Git hook failure: %v: %s", err, output)
	}
	originalGitStatus := exit.ExitCode()
	call("hooks", "install")
	if err = os.Remove(filepath.Join(repo, "private")); err != nil {
		t.Fatal(err)
	}
	runExecutable(t, git, repo, originalGitStatus, "switch", "-c", "original-failure")
	assertContents(t, filepath.Join(repo, "private"), "private fixture")
	call("hooks", "uninstall")
}

func TestHooksCLICustomConfiguration(t *testing.T) {
	base, repo, data := cliFixture(t)
	binary := buildHookCLI(t, base)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	call := func(code int, args ...string) string {
		t.Helper()
		return runExecutable(t, binary, repo, code, append([]string{"-C", repo, "--data-root", data}, args...)...)
	}
	call(0, "init")
	runExecutable(t, git, repo, 0, "config", "core.hooksPath", "custom-hooks")
	if out := call(1, "hooks", "install"); !strings.Contains(out, "core.hooksPath is configured") {
		t.Fatal("installer did not diagnose custom hook configuration")
	}
	if _, err := os.Lstat(filepath.Join(repo, ".git", "hooks")); !os.IsNotExist(err) {
		t.Fatal("refused installation created hook files")
	}
	runExecutable(t, git, repo, 0, "config", "--unset", "core.hooksPath")
	call(0, "hooks", "install")
	runExecutable(t, git, repo, 0, "config", "core.hooksPath", "custom-hooks")
	if out := call(1, "hooks", "status"); !strings.Contains(out, "State: bypassed") {
		t.Fatal("status did not diagnose a bypassed integration")
	}
	call(0, "hooks", "uninstall")
	if out := runExecutable(t, git, repo, 0, "config", "--get", "core.hooksPath"); strings.TrimSpace(out) != "custom-hooks" {
		t.Fatal("uninstall changed custom hook configuration")
	}
}
