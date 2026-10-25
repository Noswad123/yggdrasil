package store

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(gitEnvironment(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@localhost", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@localhost")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}
func writeTest(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
}
func readTest(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func setup(t *testing.T) (*Manager, string) {
	t.Helper()
	base, err := realPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(base, "source")
	if err = os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, src, "init", "--template=", "--initial-branch=main")
	writeTest(t, filepath.Join(src, "public.txt"), "public")
	gitTest(t, src, "add", "public.txt")
	gitTest(t, src, "commit", "-m", "Initial source")
	m, err := New(src, filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	return m, base
}
func requireInit(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
}
func requireTrack(t *testing.T, m *Manager, p string) {
	t.Helper()
	if err := m.Track(p); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleAcrossWorktrees(t *testing.T) {
	m, base := setup(t)
	if _, err := os.Stat(m.Root()); !os.IsNotExist(err) {
		t.Fatal("New mutated store")
	}
	requireInit(t, m)
	requireInit(t, m)
	h, err := m.History()
	if err != nil || len(h) != 0 {
		t.Fatalf("empty history: %v %v", h, err)
	}
	path := "local/settings.env"
	writeTest(t, m.source(path), "first\n")
	indexBefore := gitTest(t, m.Worktree(), "ls-files", "--stage")
	requireTrack(t, m, path)
	requireTrack(t, m, path)
	if !m.linked(path) {
		t.Fatal("missing canonical absolute symlink")
	}
	if got := readTest(t, m.central(path)); got != "first\n" {
		t.Fatal("incorrect import")
	}
	imports, err := filepath.Glob(filepath.Join(m.Root(), "imports", "*", "local", "settings.env"))
	if err != nil || len(imports) != 1 {
		t.Fatalf("original backups: %v %v", imports, err)
	}
	if readTest(t, imports[0]) != "first\n" {
		t.Fatal("original not retained")
	}
	first, err := m.Snapshot("unchanged")
	if err != nil {
		t.Fatal(err)
	}
	if d, err := m.Dirty(); err != nil || d {
		t.Fatalf("dirty after import: %v %v", d, err)
	}
	wt := filepath.Join(base, "linked")
	gitTest(t, m.Worktree(), "worktree", "add", "-b", "linked", wt)
	m2, err := New(wt, filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if m2.Root() != m.Root() {
		t.Fatal("linked worktree has different store")
	}
	if err = m2.Attach(); err != nil {
		t.Fatal(err)
	}
	if err = m2.Attach(); err != nil {
		t.Fatal(err)
	}
	writeTest(t, m2.source(path), "second\n")
	if readTest(t, m.source(path)) != "second\n" {
		t.Fatal("links are not shared")
	}
	if d, err := m.Dirty(); err != nil || !d {
		t.Fatalf("not dirty: %v %v", d, err)
	}
	second, err := m2.Snapshot("Second version")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("no new snapshot")
	}
	// Unsnapshotted newer contents must also remain recoverable.
	writeTest(t, m.source(path), "third unsaved\n")
	restored, err := m.Restore(path, first[:12])
	if err != nil {
		t.Fatal(err)
	}
	if restored == first || restored == second {
		t.Fatal("restore rewrote history")
	}
	if readTest(t, m2.source(path)) != "first\n" {
		t.Fatal("restore did not update all links")
	}
	h, err = m.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 4 || !strings.HasPrefix(h[0].Message, "Restore ") {
		t.Fatalf("unexpected history: %+v", h)
	}
	if b, err := m.git(nil, "show", h[1].ID+":files/"+path); err != nil || string(b) != "third unsaved\n" {
		t.Fatal("pre-restore checkpoint lost")
	}
	s, err := m2.Status()
	if err != nil || len(s) != 1 || s[0].State != "linked" {
		t.Fatalf("status: %+v %v", s, err)
	}
	if got := gitTest(t, m.Worktree(), "ls-files", "--stage"); got != indexBefore {
		t.Fatal("source index was modified")
	}
	if out := gitTest(t, m.Worktree(), "status", "--porcelain"); out != "" {
		t.Fatalf("source is not clean: %s", out)
	}
	gitTest(t, wt, "check-ignore", "--", path)
	if st, err := os.Stat(m.central(path)); err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("central permissions: %v %v", st, err)
	}
	for _, p := range []string{m.Root(), m.private(), filepath.Join(m.private(), "files"), filepath.Dir(m.central(path)), filepath.Join(m.Root(), "imports")} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0700 {
			t.Fatalf("directory permissions %s: %v %v", p, st, err)
		}
	}
	st, err := os.Stat(filepath.Join(m.Root(), "config.json"))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("config permissions")
	}
}

func TestTrackRefusals(t *testing.T) {
	m, _ := setup(t)
	requireInit(t, m)
	writeTest(t, m.source("secret"), "keep")
	for _, p := range []string{"public.txt", "../secret", "sub/../secret", "./secret", "/secret", ".git/config", "sub/.GIT/config", "a\nfile", "a\x00file", "sub//file", "secret/"} {
		if err := m.Track(p); err == nil {
			t.Errorf("accepted unsafe path %q", p)
		}
	}
	if err := os.Symlink(m.source("secret"), m.source("alias")); err != nil {
		t.Fatal(err)
	}
	if err := m.Track("alias"); err == nil {
		t.Fatal("accepted leaf symlink")
	}
	if err := os.Mkdir(m.source("real"), 0700); err != nil {
		t.Fatal(err)
	}
	writeTest(t, m.source("real/file"), "keep")
	if err := os.Symlink(m.source("real"), m.source("dirlink")); err != nil {
		t.Fatal(err)
	}
	if err := m.Track("dirlink/file"); err == nil {
		t.Fatal("accepted ancestor symlink")
	}
	// Staged deletion must not bypass the HEAD check.
	gitTest(t, m.Worktree(), "rm", "--cached", "public.txt")
	if err := m.Track("public.txt"); err == nil {
		t.Fatal("accepted file still tracked in HEAD")
	}
	writeTest(t, m.source(".gitignore"), "secret\n")
	gitTest(t, m.Worktree(), "add", "-f", "secret")
	if err := m.Track("secret"); err == nil {
		t.Fatal("accepted ignored but tracked file")
	}
	if readTest(t, m.source("secret")) != "keep" {
		t.Fatal("refusal modified original")
	}
}

func TestAttachPreflightAndConflicts(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	for _, p := range []string{"a/file", "z/file"} {
		writeTest(t, m.source(p), p)
		requireTrack(t, m, p)
	}
	wt := filepath.Join(base, "linked")
	gitTest(t, m.Worktree(), "worktree", "add", "-b", "other", wt)
	m2, err := New(wt, filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, m2.source("z/file"), "do not clobber")
	if err = m2.Attach(); err == nil {
		t.Fatal("accepted regular conflict")
	}
	if _, err = os.Lstat(m2.source("a")); !os.IsNotExist(err) {
		t.Fatal("preflight wrote first path")
	}
	if readTest(t, m2.source("z/file")) != "do not clobber" {
		t.Fatal("clobbered conflict")
	}
	if err = os.Remove(m2.source("z/file")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(m.source("z/file"), m2.source("z/file")); err != nil {
		t.Fatal(err)
	}
	if err = m2.Attach(); err == nil {
		t.Fatal("accepted unexpected symlink")
	}
	if err = os.Remove(m2.source("z/file")); err != nil {
		t.Fatal(err)
	}
	if err = m2.Attach(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(m.source("a/file")); err != nil {
		t.Fatal(err)
	}
	writeTest(t, m.source("a/file"), "conflict")
	if err = m.Track("a/file"); err == nil {
		t.Fatal("managed regular file accepted")
	}
	s, err := m.Status()
	if err != nil || s[0].State != "conflict" {
		t.Fatalf("status %+v %v", s, err)
	}
	if err = os.Remove(m.central("z/file")); err != nil {
		t.Fatal(err)
	}
	if err = m2.Attach(); err == nil {
		t.Fatal("accepted missing central file")
	}
	if _, err = m.Snapshot("missing"); err == nil {
		t.Fatal("snapshot accepted missing file")
	}
}

func TestOverlapAndCanonicalLocation(t *testing.T) {
	m, base := setup(t)
	wt := filepath.Join(base, "linked")
	gitTest(t, m.Worktree(), "worktree", "add", "-b", "other", wt)
	for _, root := range []string{m.Worktree(), filepath.Join(m.Worktree(), "data"), wt, filepath.Join(wt, "data"), m.common, filepath.Join(m.common, "data"), base} {
		if _, err := New(m.Worktree(), root); err == nil {
			t.Errorf("accepted overlapping root %s", root)
		}
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(m.Worktree(), alias); err != nil {
		t.Fatal(err)
	}
	fromAlias, err := New(alias, filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if fromAlias.Root() != m.Root() || fromAlias.Worktree() != m.Worktree() {
		t.Fatal("location not canonical")
	}
	if _, err := New(m.common, filepath.Join(base, "data")); err == nil {
		t.Fatal("accepted Git metadata as worktree")
	}
	bare := filepath.Join(base, "bare")
	gitTest(t, base, "init", "--bare", bare)
	if _, err := New(bare, filepath.Join(base, "data")); err == nil {
		t.Fatal("accepted bare repo")
	}
}

func TestLocksAndMalformedStore(t *testing.T) {
	m, _ := setup(t)
	requireInit(t, m)
	writeTest(t, m.source("secret"), "original")
	unlock, err := m.lock()
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Init(); err == nil {
		t.Fatal("Init ignored lock")
	}
	if err = m.Track("secret"); err == nil {
		t.Fatal("Track ignored lock")
	}
	if err = m.Attach(); err == nil {
		t.Fatal("Attach ignored lock")
	}
	if _, err = m.Snapshot("test"); err == nil {
		t.Fatal("Snapshot ignored lock")
	}
	if _, err = m.Restore("secret", "1234"); err == nil {
		t.Fatal("Restore ignored lock")
	}
	if _, err = m.Status(); err != nil {
		t.Fatal("read-only status blocked by lock", err)
	}
	unlock()
	writeTest(t, filepath.Join(m.Root(), "config.json"), `{"version":1,"source_common_dir":"wrong","managed_paths":[]}`)
	if err = m.Init(); err == nil {
		t.Fatal("accepted mismatched store")
	}
	if err = m.Track("secret"); err == nil {
		t.Fatal("Track accepted malformed store")
	}
	if readTest(t, m.source("secret")) != "original" {
		t.Fatal("modified original on failure")
	}
}

func TestGitEnvironmentHooksAndAllowlist(t *testing.T) {
	m, base := setup(t)
	foreign := filepath.Join(base, "foreign")
	gitTest(t, base, "init", "--bare", foreign)
	t.Setenv("GIT_DIR", foreign)
	t.Setenv("GIT_WORK_TREE", base)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(base, "foreign-index"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", filepath.Join(base, "hooks"))
	t.Setenv("GIT_AUTHOR_NAME", "")
	t.Setenv("GIT_COMMITTER_EMAIL", "")
	m2, err := New(m.Worktree(), filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	requireInit(t, m2)
	marker := filepath.Join(base, "hook-ran")
	for _, hook := range []string{"pre-commit", "post-commit", "reference-transaction"} {
		p := filepath.Join(m.private(), ".git", "hooks", hook)
		writeTest(t, p, "#!/bin/sh\ntouch '"+marker+"'\nexit 1\n")
		if err = os.Chmod(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeTest(t, m.source("secret"), "value")
	requireTrack(t, m, "secret")
	writeTest(t, filepath.Join(m.private(), "not-allowed"), "must not enter history")
	writeTest(t, filepath.Join(m.private(), "files", "extra"), "must not enter history")
	writeTest(t, filepath.Join(m.private(), ".gitattributes"), "* filter=evil\n")
	gitTest(t, m.private(), "config", "filter.evil.clean", "false")
	gitTest(t, m.private(), "config", "commit.gpgsign", "true")
	gitTest(t, m.private(), "config", "gpg.program", "false")
	writeTest(t, m.source("secret"), "updated")
	if _, err = m.Snapshot("checkpoint"); err != nil {
		t.Fatal(err)
	}
	b, err := m.git(nil, "ls-tree", "-r", "--name-only", "HEAD")
	if err != nil || string(b) != "files/secret\n" {
		t.Fatalf("nonallowlisted tree: %s %v", b, err)
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("hook executed")
	}
	if _, err = os.Stat(filepath.Join(base, "foreign-index")); !os.IsNotExist(err) {
		t.Fatal("inherited index contaminated")
	}
}

func TestIgnoreEscapingAndNoDuplication(t *testing.T) {
	m, _ := setup(t)
	requireInit(t, m)
	p := "a [literal]*?!# .env "
	writeTest(t, m.source(p), "value")
	requireTrack(t, m, p)
	exclude := gitTest(t, m.Worktree(), "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	before := readTest(t, exclude)
	if err := m.Attach(); err != nil {
		t.Fatal(err)
	}
	requireTrack(t, m, p)
	if readTest(t, exclude) != before {
		t.Fatal("duplicate ignore entries")
	}
	gitTest(t, m.Worktree(), "check-ignore", "--", p)
}

func TestRestoreValidationAndSymlinkCapture(t *testing.T) {
	m, _ := setup(t)
	requireInit(t, m)
	writeTest(t, m.source("one"), "one")
	requireTrack(t, m, "one")
	h, err := m.History()
	if err != nil {
		t.Fatal(err)
	}
	old := h[0].ID
	writeTest(t, m.source("two"), "two")
	requireTrack(t, m, "two")
	for _, rev := range []string{"HEAD", "--help", old + "~1", old + ":files/one", "123", strings.Repeat("f", 40)} {
		if _, err = m.Restore("one", rev); err == nil {
			t.Errorf("accepted revision %q", rev)
		}
	}
	if _, err = m.Restore("unknown", old); err == nil {
		t.Fatal("accepted unmanaged file")
	}
	if _, err = m.Restore("two", old); err == nil {
		t.Fatal("accepted missing historical file")
	}
	if err = os.Remove(m.central("one")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(m.source("public.txt"), m.central("one")); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Snapshot("unsafe"); err == nil {
		t.Fatal("followed central symlink")
	}
	if _, err = m.Restore("one", old); err == nil {
		t.Fatal("restore followed central symlink")
	}
	if readTest(t, m.source("public.txt")) != "public" {
		t.Fatal("modified symlink destination")
	}
}

func TestFailedImportRetainsOriginal(t *testing.T) {
	m, _ := setup(t)
	requireInit(t, m)
	writeTest(t, m.source("secret"), "recover me")
	// Fail after the initial central copy and snapshot, but before moving source.
	exclude := gitTest(t, m.Worktree(), "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	if err := os.Remove(exclude); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(exclude), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(exclude, 0700); err != nil {
		t.Fatal(err)
	}
	if err := m.Track("secret"); err == nil {
		t.Fatal("expected ignore failure")
	}
	if readTest(t, m.source("secret")) != "recover me" {
		t.Fatal("original lost")
	}
	c, err := m.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Paths) != 0 {
		t.Fatal("config silently claimed partial import")
	}
	if readTest(t, m.central("secret")) != "recover me" {
		t.Fatal("central recovery copy missing")
	}
	if err = m.Track("secret"); err == nil {
		t.Fatal("silently overwrote partial import")
	}
	b, err := m.git(nil, "show", "HEAD:files/secret")
	if err != nil || !bytes.Equal(b, []byte("recover me")) {
		t.Fatal("recovery checkpoint missing")
	}
}

func TestCommonIndexAndTrackedAttachRefused(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	wt := filepath.Join(base, "linked")
	gitTest(t, m.Worktree(), "worktree", "add", "-b", "other", wt)
	m2, err := New(wt, filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, m.source("staged-only"), "public staged")
	gitTest(t, m.Worktree(), "add", "staged-only")
	writeTest(t, m2.source("staged-only"), "keep private")
	if err = m2.Track("staged-only"); err == nil {
		t.Fatal("ignored common/main index")
	}
	writeTest(t, m.source("private"), "private value")
	requireTrack(t, m, "private")
	writeTest(t, m2.source("private"), "public conflict")
	gitTest(t, wt, "add", "-f", "private")
	if err = m2.Attach(); err == nil {
		t.Fatal("attached over tracked path")
	}
	h, err := m.History()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m2.Restore("private", h[0].ID); err == nil {
		t.Fatal("restored despite tracked conflict")
	}
	s, err := m2.Status()
	if err != nil || s[0].State != "tracked" {
		t.Fatalf("tracked status: %+v %v", s, err)
	}
}

func TestPrivateMetadataAndParentSymlinksRefused(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	writeTest(t, m.source("dir/file"), "private")
	requireTrack(t, m, "dir/file")
	wt := filepath.Join(base, "linked")
	gitTest(t, m.Worktree(), "worktree", "add", "-b", "other", wt)
	m2, err := New(wt, filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(m.Worktree(), "dir"), filepath.Join(wt, "dir")); err != nil {
		t.Fatal(err)
	}
	if err = m2.Attach(); err == nil {
		t.Fatal("Attach followed ancestor symlink")
	}
	if err = os.RemoveAll(filepath.Join(m.private(), ".git", "objects")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(m.common, "objects"), filepath.Join(m.private(), ".git", "objects")); err != nil {
		t.Fatal(err)
	}
	if err = m.Init(); err == nil {
		t.Fatal("Init accepted symlinked Git metadata")
	}
	if _, err = m.Snapshot("bad metadata"); err == nil {
		t.Fatal("Snapshot accepted symlinked Git metadata")
	}
}

func TestIneffectiveIgnoreRetainsOriginal(t *testing.T) {
	m, _ := setup(t)
	requireInit(t, m)
	writeTest(t, m.source(".gitignore"), "!secret\n")
	writeTest(t, m.source("secret"), "recover me")
	if err := m.Track("secret"); err == nil {
		t.Fatal("accepted ineffective local exclude")
	}
	if err := regular(m.source("secret")); err != nil {
		t.Fatal("original was replaced", err)
	}
	if readTest(t, m.source("secret")) != "recover me" {
		t.Fatal("original changed")
	}
	c, err := m.load()
	if err != nil || len(c.Paths) != 0 {
		t.Fatalf("partial import claimed: %+v %v", c, err)
	}
}

func TestRestoreResolvesObjectsNotHexRefs(t *testing.T) {
	m, _ := setup(t)
	requireInit(t, m)
	writeTest(t, m.source("file"), "first")
	requireTrack(t, m, "file")
	h, err := m.History()
	if err != nil {
		t.Fatal(err)
	}
	first := h[0].ID
	writeTest(t, m.source("file"), "second")
	second, err := m.Snapshot("second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.git(nil, "update-ref", "refs/heads/"+first[:12], second); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Restore("file", first[:12]); err != nil {
		t.Fatal(err)
	}
	if readTest(t, m.source("file")) != "first" {
		t.Fatal("hex ref overrode object prefix")
	}
}
