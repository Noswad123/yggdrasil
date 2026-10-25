package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func invoke(t *testing.T, expected int, args ...string) string {
	t.Helper()
	var out, stderr bytes.Buffer
	if code := run(args, &out, &stderr); code != expected {
		t.Fatalf("run %v: exit %d, want %d; stdout=%s stderr=%s", args, code, expected, &out, &stderr)
	}
	return out.String() + stderr.String()
}

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	for _, e := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(e), "GIT_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@localhost",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@localhost")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func assertContents(t *testing.T, path, contents string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != contents {
		t.Fatalf("unexpected contents in %s", path)
	}
}

func cliFixture(t *testing.T) (string, string, string) {
	t.Helper()
	base := t.TempDir()
	repo := filepath.Join(base, "source repo")
	data := filepath.Join(base, "private data")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "init", "--template=", "--initial-branch=main")
	writeFile(t, filepath.Join(repo, "public"), "source controlled\n")
	testGit(t, repo, "add", "public")
	testGit(t, repo, "commit", "-m", "Initial source")
	return base, repo, data
}

func TestHelpAndInvalidArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}} {
		if out := invoke(t, 0, args...); !strings.Contains(out, "Yggdrasil") {
			t.Fatal("missing help")
		}
	}
	for _, args := range [][]string{
		{"not-a-command"}, {"track"}, {"attach", "extra"}, {"init", "extra"},
		{"restore", "file"}, {"track", "file", "--dry-run"},
	} {
		invoke(t, 1, args...)
	}
	invoke(t, 2, "--unknown")
	invoke(t, 2, "snapshot", "--unknown")
	for _, command := range []string{"track", "attach", "restore"} {
		invoke(t, 2, command, "--yes")
	}
	// Per-command help must work without locating a source repository.
	invoke(t, 0, "track", "--help")
}

func TestDefaultDataRoot(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/example/data")
	root, err := defaultDataRoot()
	if err != nil || root != "/example/data/yggdrasil/repos" {
		t.Fatalf("XDG root: %s %v", root, err)
	}
	t.Setenv("XDG_DATA_HOME", "relative")
	if _, err := defaultDataRoot(); err == nil {
		t.Fatal("accepted relative XDG_DATA_HOME")
	}
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/example/home")
	root, err = defaultDataRoot()
	if err != nil || root != "/example/home/.local/share/yggdrasil/repos" {
		t.Fatalf("home root: %s %v", root, err)
	}
}

func TestUninitializedCommandsDoNotCreateStore(t *testing.T) {
	_, repo, data := cliFixture(t)
	for _, command := range [][]string{
		{"status"}, {"history"}, {"attach"}, {"attach", "--dry-run"},
		{"track", "file"}, {"track", "--dry-run", "file"}, {"snapshot"},
	} {
		args := append([]string{"-C", repo, "--data-root", data}, command...)
		out := invoke(t, 1, args...)
		if !strings.Contains(out, "run ygg init first") {
			t.Fatal("missing initialization guidance")
		}
	}
	if _, err := os.Lstat(data); !os.IsNotExist(err) {
		t.Fatal("uninitialized command created store")
	}
}

func TestCLIWorktreeLifecycle(t *testing.T) {
	base, repo, data := cliFixture(t)
	const file = ".env.ygg-test"
	const initial = "DUMMY_SECRET=initial-private-value\n"
	const updated = "DUMMY_SECRET=updated-private-value\n"
	var transcript strings.Builder
	call := func(dir string, code int, command ...string) string {
		t.Helper()
		args := append([]string{"-C", dir, "--data-root", data}, command...)
		out := invoke(t, code, args...)
		transcript.WriteString(out)
		return out
	}
	call(repo, 0, "init")
	call(repo, 0, "init")
	call(repo, 0, "status")
	writeFile(t, filepath.Join(repo, file), initial)
	if out := call(repo, 0, "track", "--dry-run", file); !strings.Contains(out, "Preview only") {
		t.Fatal("missing preview")
	}
	if st, err := os.Lstat(filepath.Join(repo, file)); err != nil || !st.Mode().IsRegular() {
		t.Fatal("preview replaced original")
	}
	if out := call(repo, 0, "history"); !strings.Contains(out, "No snapshots") {
		t.Fatal("preview created snapshot")
	}
	call(repo, 0, "track", file)
	if st, err := os.Lstat(filepath.Join(repo, file)); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("default track did not install shared link")
	}
	call(repo, 0, "track", file)
	firstHistory := call(repo, 0, "history")
	firstID := strings.Fields(firstHistory)[0]
	linked := filepath.Join(base, "feature worktree")
	testGit(t, repo, "worktree", "add", "-b", "feature", linked)
	if out := call(linked, 1, "status"); !strings.Contains(out, "missing") {
		t.Fatal("new worktree should need attachment")
	}
	if out := call(linked, 0, "attach", "--dry-run"); !strings.Contains(out, "Preview only") {
		t.Fatal("missing attach preview")
	}
	if _, err := os.Lstat(filepath.Join(linked, file)); !os.IsNotExist(err) {
		t.Fatal("attach preview created link")
	}
	call(linked, 0, "attach")
	call(linked, 0, "attach")
	writeFile(t, filepath.Join(linked, file), updated)
	assertContents(t, filepath.Join(repo, file), updated)
	if out := call(repo, 0, "status"); !strings.Contains(out, "uncaptured changes;") {
		t.Fatal("missing dirty status")
	}
	call(linked, 0, "snapshot", "-m", "Working dummy configuration")
	historyBeforeRestore := call(repo, 0, "history")
	if out := call(repo, 0, "restore", "--snapshot", firstID, "--dry-run", file); !strings.Contains(out, "Preview only") {
		t.Fatal("missing restore preview")
	}
	assertContents(t, filepath.Join(linked, file), updated)
	if out := call(repo, 0, "history"); out != historyBeforeRestore {
		t.Fatal("restore preview changed history")
	}
	call(repo, 0, "restore", "--snapshot", firstID, file)
	assertContents(t, filepath.Join(linked, file), initial)
	if out := call(repo, 0, "history"); strings.Count(out, "\n") != 3 {
		t.Fatal("restore should add history, not reset it")
	}
	if out := call(repo, 0, "status"); !strings.Contains(out, "no uncaptured changes") {
		t.Fatal("restore left dirty history")
	}
	for _, dir := range []string{repo, linked} {
		if out := testGit(t, dir, "status", "--porcelain"); out != "" {
			t.Fatal("private attachment dirtied source repository")
		}
		testGit(t, dir, "check-ignore", "--", file)
		if tracked := testGit(t, dir, "ls-files"); tracked != "public" {
			t.Fatal("private file entered source index")
		}
	}
	// Removing a worktree must leave the shared target and history intact.
	testGit(t, repo, "worktree", "remove", "--force", linked)
	assertContents(t, filepath.Join(repo, file), initial)
	call(repo, 0, "history")
	if strings.Contains(transcript.String(), initial) || strings.Contains(transcript.String(), updated) {
		t.Fatal("CLI printed private file contents")
	}
}

func TestCLIRefusesTrackedPathsAndDetachedLinks(t *testing.T) {
	_, repo, data := cliFixture(t)
	call := func(code int, command ...string) string {
		t.Helper()
		return invoke(t, code, append([]string{"-C", repo, "--data-root", data}, command...)...)
	}
	call(0, "init")
	call(1, "track", "public")
	call(1, "track", "--dry-run", "public")
	assertContents(t, filepath.Join(repo, "public"), "source controlled\n")
	path := filepath.Join(repo, "private")
	writeFile(t, path, "shared fixture")
	call(0, "track", "private")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "detached fixture")
	if out := call(1, "status"); !strings.Contains(out, "conflict") {
		t.Fatal("missing detached-link warning")
	}
	call(1, "attach")
	call(1, "attach", "--dry-run")
	assertContents(t, path, "detached fixture")
}

func TestCLIDirectoryWorktreeLifecycle(t *testing.T) {
	base, repo, data := cliFixture(t)
	const dir = "apps/ix-bap-playwright/.local/shared"
	const first = "initial-private-directory-fixture"
	const future = "future-private-directory-fixture"
	var transcript strings.Builder
	call := func(worktree string, code int, command ...string) string {
		t.Helper()
		out := invoke(t, code, append([]string{"-C", worktree, "--data-root", data}, command...)...)
		transcript.WriteString(out)
		return out
	}
	call(repo, 0, "init")
	for _, child := range []string{"config", "empty/nested"} {
		if err := os.MkdirAll(filepath.Join(repo, dir, child), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(repo, dir, "config/settings.json"), first)
	for _, spelling := range []string{dir, dir + "/"} {
		out := call(repo, 0, "track", "--dry-run", spelling)
		if !strings.Contains(out, "Preview only") || !strings.Contains(out, "Entire folder is shared") {
			t.Fatal("folder preview must explain subtree sharing")
		}
	}
	if st, err := os.Lstat(filepath.Join(repo, dir)); err != nil || !st.IsDir() {
		t.Fatal("directory preview replaced original")
	}
	if out := call(repo, 0, "history"); !strings.Contains(out, "No snapshots yet") {
		t.Fatal("directory preview created history")
	}
	if _, err := os.Lstat(filepath.Join(repo, ".git", "info", "exclude")); !os.IsNotExist(err) {
		t.Fatal("directory preview changed local excludes")
	}
	call(repo, 0, "track", dir+"/")
	call(repo, 0, "track", dir)
	call(repo, 0, "track", dir+"/")
	if st, err := os.Lstat(filepath.Join(repo, dir)); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("default folder tracking did not install one root link")
	}
	initialHistory := call(repo, 0, "history")
	initialID := strings.Fields(initialHistory)[0]
	linked := filepath.Join(base, "folder worktree")
	testGit(t, repo, "worktree", "add", "-b", "folders", linked)
	call(linked, 0, "attach", "--dry-run")
	if _, err := os.Lstat(filepath.Join(linked, dir)); !os.IsNotExist(err) {
		t.Fatal("folder attach preview created path")
	}
	call(linked, 0, "attach")
	call(linked, 0, "attach")
	assertContents(t, filepath.Join(linked, dir, "config/settings.json"), first)
	// New files and replaced children stay shared through the folder root link.
	if err := os.MkdirAll(filepath.Join(linked, dir, "fixtures"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(linked, dir, "fixtures/new.json"), future)
	assertContents(t, filepath.Join(repo, dir, "fixtures/new.json"), future)
	if err := os.Rename(filepath.Join(linked, dir, "config/settings.json"), filepath.Join(linked, dir, "config/renamed.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(linked, dir, "empty/nested")); err != nil {
		t.Fatal(err)
	}
	if out := call(repo, 0, "status"); !strings.Contains(out, "uncaptured changes;") {
		t.Fatal("folder additions/deletions not reported dirty")
	}
	call(repo, 0, "snapshot", "-m", "Updated shared folder")
	writeFile(t, filepath.Join(linked, dir, "uncaptured.json"), future)
	historyBefore := call(repo, 0, "history")
	if out := call(repo, 0, "restore", "--dry-run", "--snapshot", initialID, dir+"/"); !strings.Contains(out, "newer children are removed") {
		t.Fatal("folder restore preview must explain replacement")
	}
	assertContents(t, filepath.Join(linked, dir, "uncaptured.json"), future)
	if out := call(repo, 0, "history"); out != historyBefore {
		t.Fatal("folder restore preview changed history")
	}
	call(repo, 0, "restore", "--snapshot", initialID, dir+"/")
	assertContents(t, filepath.Join(linked, dir, "config/settings.json"), first)
	for _, path := range []string{"fixtures", "uncaptured.json", "config/renamed.json"} {
		if _, err := os.Lstat(filepath.Join(linked, dir, path)); !os.IsNotExist(err) {
			t.Fatal("folder restore merged instead of replacing subtree")
		}
	}
	if st, err := os.Stat(filepath.Join(linked, dir, "empty/nested")); err != nil || !st.IsDir() {
		t.Fatal("folder restore lost empty directories")
	}
	if out := call(repo, 0, "history"); strings.Count(out, "\n") != 4 {
		t.Fatal("folder restore did not retain uncaptured changes in history")
	}
	for _, worktree := range []string{repo, linked} {
		call(worktree, 0, "status")
		if out := testGit(t, worktree, "status", "--porcelain"); out != "" {
			t.Fatal("folder attachment dirtied source Git")
		}
		if out := testGit(t, worktree, "ls-files"); out != "public" {
			t.Fatal("folder attachment changed source index")
		}
	}
	testGit(t, repo, "worktree", "remove", "--force", linked)
	assertContents(t, filepath.Join(repo, dir, "config/settings.json"), first)
	if strings.Contains(transcript.String(), first) || strings.Contains(transcript.String(), future) {
		t.Fatal("CLI printed private subtree contents")
	}
}
