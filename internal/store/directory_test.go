package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func mkdirTest(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
}

func dirtyTest(t *testing.T, m *Manager, want bool) {
	t.Helper()
	got, err := m.Dirty()
	if err != nil || got != want {
		t.Fatalf("dirty = %v, want %v: %v", got, want, err)
	}
}

func TestDirectoryLifecycleAcrossWorktrees(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	p := "local/shared"
	writeTest(t, m.source(p+"/nested/first"), "first private fixture")
	mkdirTest(t, m.source(p+"/empty/deeper"))
	if err := os.Chmod(m.source(p), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(m.source(p+"/nested/first"), 0640); err != nil {
		t.Fatal(err)
	}
	original := filesystemState(t, m.source(p))
	index := gitTest(t, m.Worktree(), "ls-files", "--stage")
	noPlanMutation(t, base, func() {
		for _, spelling := range []string{p, p + "/"} {
			plan, err := m.PlanTrack(spelling)
			if err != nil || plan.Kind != "directory" || plan.Source != m.source(p) || plan.Target != m.central(p) || plan.AlreadyManaged {
				t.Fatalf("directory plan: %+v %v", plan, err)
			}
		}
	})
	requireTrack(t, m, p+"/")
	requireTrack(t, m, p)
	requireTrack(t, m, p+"/")
	if !m.linked(p) {
		t.Fatal("directory root is not a canonical link")
	}
	c, err := m.load()
	if err != nil || c.Version != 2 || len(c.Paths) != 1 || c.Paths[0] != p || c.kind(p) != "directory" {
		t.Fatalf("directory config: %+v %v", c, err)
	}
	backups, err := filepath.Glob(filepath.Join(m.root, "imports", "*", filepath.FromSlash(p)))
	if err != nil || len(backups) != 1 {
		t.Fatalf("original directory backup: %v", err)
	}
	if !reflect.DeepEqual(original, filesystemState(t, backups[0])) {
		t.Fatal("original directory shape, bytes, permissions or timestamps changed")
	}
	err = filepath.WalkDir(m.central(p), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		st, err := os.Lstat(path)
		if err != nil {
			return err
		}
		want := os.FileMode(0600)
		if d.IsDir() {
			want = 0700
		}
		if st.Mode().Perm() != want {
			t.Error("private copy has incorrect permissions")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.Snapshot("unchanged directory")
	if err != nil {
		t.Fatal(err)
	}
	h, err := m.History()
	if err != nil || len(h) != 1 || h[0].ID != first {
		t.Fatal("unchanged directory snapshot not reused", err)
	}
	dirtyTest(t, m, false)
	wt := filepath.Join(base, "linked")
	gitTest(t, m.Worktree(), "worktree", "add", "-b", "directory", wt)
	m2, err := New(wt, filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	noPlanMutation(t, base, func() {
		if _, err := m2.PlanAttach(); err != nil {
			t.Fatal(err)
		}
		plan, err := m.PlanTrack(p + "/")
		if err != nil || !plan.AlreadyManaged || plan.Kind != "directory" {
			t.Fatal("managed directory plan", err)
		}
		if err := m.PlanRestore(p+"/", first); err != nil {
			t.Fatal(err)
		}
	})
	if err = m2.Attach(); err != nil {
		t.Fatal(err)
	}
	if err = m2.Attach(); err != nil {
		t.Fatal(err)
	}
	writeTest(t, m2.source(p+"/future/nested/new"), "future private fixture")
	if readTest(t, m.source(p+"/future/nested/new")) != "future private fixture" {
		t.Fatal("future nested child is not shared")
	}
	if err = os.Rename(m2.source(p+"/nested/first"), m2.source(p+"/nested/renamed")); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(m.source(p + "/nested/first")); !os.IsNotExist(err) {
		t.Fatal("rename is not shared")
	}
	if err = os.Remove(m.source(p + "/future/nested/new")); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(m2.source(p + "/future/nested/new")); !os.IsNotExist(err) {
		t.Fatal("deletion is not shared")
	}
	if err = os.Remove(m.source(p + "/empty/deeper")); err != nil {
		t.Fatal(err)
	}
	dirtyTest(t, m, true)
	second, err := m2.Snapshot("Renamed and deleted directory children")
	if err != nil || second == first {
		t.Fatal("directory changes not snapshotted", err)
	}
	dirtyTest(t, m, false)
	writeTest(t, m.source("unrelated"), "other mapping")
	requireTrack(t, m, "unrelated")
	writeTest(t, m.source("unrelated"), "other pending edit")
	writeTest(t, m.source(p+"/newer"), "unsnapshotted fixture")
	if _, err = m.Restore(p+"/nested", first); err == nil {
		t.Fatal("restored non-root descendant")
	}
	if _, err = m.Restore(p+"/", first[:12]); err != nil {
		t.Fatal(err)
	}
	if readTest(t, m2.source(p+"/nested/first")) != "first private fixture" {
		t.Fatal("directory restore is not shared")
	}
	for _, child := range []string{"newer", "nested/renamed", "future"} {
		if _, err = os.Lstat(m.source(p + "/" + child)); !os.IsNotExist(err) {
			t.Errorf("restore retained newer child %s", child)
		}
	}
	if st, err := os.Stat(m.source(p + "/empty/deeper")); err != nil || !st.IsDir() {
		t.Fatal("historical empty directory missing", err)
	}
	if readTest(t, m.source("unrelated")) != "other pending edit" {
		t.Fatal("restore changed unrelated mapping")
	}
	h, err = m.History()
	if err != nil || len(h) < 2 {
		t.Fatal(err)
	}
	if _, err = m.git(nil, "cat-file", "-e", h[1].ID+":files/"+p+"/newer"); err != nil {
		t.Fatal("pre-restore checkpoint lost", err)
	}
	retained, err := filepath.Glob(filepath.Join(m.root, "restore-*", "previous", "newer"))
	if err != nil || len(retained) != 1 {
		t.Fatal("previous directory root not retained", err)
	}
	dirtyTest(t, m, false)
	if got := gitTest(t, m.Worktree(), "ls-files", "--stage"); got != index {
		t.Fatal("directory tracking changed source index")
	}
	for _, manager := range []*Manager{m, m2} {
		if out := gitTest(t, manager.Worktree(), "status", "--porcelain"); out != "" {
			t.Fatal("source worktree not clean")
		}
		gitTest(t, manager.Worktree(), "check-ignore", "--", p)
	}
	log, err := m.git(nil, "log", "--format=%s", "HEAD")
	if err != nil || strings.Contains(string(log), "private fixture") || strings.Contains(string(log), "pending edit") {
		t.Fatal("private contents leaked into history messages", err)
	}
}

func TestEmptyDirectoryChangesAreDirtyAndRestorable(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	mkdirTest(t, m.source("shared/empty/nested"))
	requireTrack(t, m, "shared")
	first, err := m.Snapshot("empty tree")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(m.source("shared/empty/nested")); err != nil {
		t.Fatal(err)
	}
	noPlanMutation(t, base, func() { dirtyTest(t, m, true) })
	second, err := m.Snapshot("empty deletion")
	if err != nil || second == first {
		t.Fatal("empty deletion lost", err)
	}
	mkdirTest(t, m.source("shared/new/empty"))
	noPlanMutation(t, base, func() { dirtyTest(t, m, true) })
	if _, err = m.Restore("shared", first); err != nil {
		t.Fatal(err)
	}
	dirtyTest(t, m, false)
	if _, err = os.Stat(m.source("shared/empty/nested")); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(m.source("shared/new")); !os.IsNotExist(err) {
		t.Fatal("restore merged empty directories")
	}
	if _, err = m.Restore("shared", second); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(m.source("shared/empty/nested")); !os.IsNotExist(err) {
		t.Fatal("restore missed empty directory deletion")
	}
}

func TestDirectoryUnsafeChildrenRefused(t *testing.T) {
	for _, unsafe := range []string{"root-marker", "nested-marker", "root-ignore", "nested-ignore", "symlink", "dangling-link", "fifo", "socket"} {
		t.Run(unsafe, func(t *testing.T) {
			m, base := setup(t)
			requireInit(t, m)
			writeTest(t, m.source("shared/safe/file"), "private fixture")
			addUnsafe := func(root string) func() {
				p := filepath.Join(root, "unsafe")
				switch unsafe {
				case "root-marker":
					writeTest(t, filepath.Join(root, ".git"), "gitdir: nowhere\n")
				case "nested-marker":
					mkdirTest(t, filepath.Join(root, "safe", ".GIT"))
				case "root-ignore":
					writeTest(t, filepath.Join(root, ".gitignore"), "private\n")
				case "nested-ignore":
					writeTest(t, filepath.Join(root, "safe", ".GitIgnore"), "private\n")
				case "symlink":
					if err := os.Symlink(m.source("public.txt"), p); err != nil {
						t.Fatal(err)
					}
				case "dangling-link":
					if err := os.Symlink("missing", p); err != nil {
						t.Fatal(err)
					}
				case "fifo":
					if err := syscall.Mkfifo(p, 0600); err != nil {
						t.Fatal(err)
					}
				case "socket":
					// A Unix socket is represented by an actual filesystem node;
					// avoid long temporary socket path limits on Darwin.
					fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
					if err != nil {
						t.Fatal(err)
					}
					if err = syscall.Bind(fd, &syscall.SockaddrUnix{Name: p}); err != nil {
						syscall.Close(fd)
						t.Skip("temporary socket path unavailable")
					}
					return func() { syscall.Close(fd) }
				}
				return func() {}
			}
			cleanup := addUnsafe(m.source("shared"))
			defer cleanup()
			// Do not inventory socket/FIFO bytes in the test fixture helper.
			noPlanMutation(t, base, func() {
				if _, err := m.PlanTrack("shared/"); err == nil {
					t.Error("planned unsafe directory")
				}
			})
			noMutationExceptLockDirectory(t, m, base, func() {
				if err := m.Track("shared"); err == nil {
					t.Error("imported unsafe directory")
				}
			})
			if _, err := os.Lstat(m.central("shared")); !os.IsNotExist(err) {
				t.Fatal("unsafe import copied before refusal")
			}
		})
	}
}

func TestUnsafeAdditionsRevalidatedForEveryOperation(t *testing.T) {
	for _, child := range []string{"nested/.git/config", "nested/.gitignore", "link"} {
		t.Run(child, func(t *testing.T) {
			m, base := setup(t)
			requireInit(t, m)
			writeTest(t, m.source("shared/safe"), "private fixture")
			requireTrack(t, m, "shared")
			h, err := m.History()
			if err != nil {
				t.Fatal(err)
			}
			if child == "link" {
				if err = os.Symlink(m.source("public.txt"), m.central("shared/link")); err != nil {
					t.Fatal(err)
				}
			} else {
				writeTest(t, m.central("shared/"+child), "unsafe fixture")
			}
			noPlanMutation(t, base, func() {
				if _, err := m.PlanTrack("shared/"); err == nil {
					t.Error("planned unsafe managed tree")
				}
				if _, err := m.PlanAttach(); err == nil {
					t.Error("planned unsafe attachment")
				}
				if err := m.PlanRestore("shared", h[0].ID); err == nil {
					t.Error("planned restore with unsafe live tree")
				}
				if _, err := m.Dirty(); err == nil {
					t.Error("dirty accepted unsafe tree")
				}
				s, err := m.Status()
				if err == nil && (len(s) != 1 || s[0].State == "linked") {
					t.Error("status accepted unsafe tree")
				}
			})
			noMutationExceptLockDirectory(t, m, base, func() {
				if err := m.Attach(); err == nil {
					t.Error("attached unsafe tree")
				}
				if _, err := m.Restore("shared", h[0].ID); err == nil {
					t.Error("restored unsafe tree")
				}
				if _, err := m.Snapshot("unsafe"); err == nil {
					t.Error("snapshot accepted unsafe tree")
				}
			})
		})
	}
}

func TestDirectoryAttachPreflightsAllMappings(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	mkdirTest(t, m.source("a/shared"))
	mkdirTest(t, m.source("z/shared"))
	requireTrack(t, m, "a/shared")
	requireTrack(t, m, "z/shared")
	wt := filepath.Join(base, "linked")
	gitTest(t, m.Worktree(), "worktree", "add", "-b", "conflict-directory", wt)
	m2, err := New(wt, filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []bool{false, true} {
		if directory {
			mkdirTest(t, m2.source("z/shared"))
		} else {
			writeTest(t, m2.source("z/shared"), "existing fixture")
		}
		noPlanMutation(t, base, func() {
			if _, err := m2.PlanAttach(); err == nil {
				t.Error("planned directory conflict")
			}
		})
		noMutationExceptLockDirectory(t, m2, base, func() {
			if err := m2.Attach(); err == nil {
				t.Error("attached directory conflict")
			}
		})
		if _, err = os.Lstat(m2.source("a")); !os.IsNotExist(err) {
			t.Fatal("attach partially wrote earlier mapping")
		}
		if err = os.Remove(m2.source("z/shared")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDirectoryTrackedDescendantsRefused(t *testing.T) {
	for _, ownership := range []string{"indexed", "HEAD-staged-deletion", "absent-indexed", "aliased-absent-indexed"} {
		t.Run(ownership, func(t *testing.T) {
			m, base := setup(t)
			requireInit(t, m)
			writeTest(t, m.source("shared/private"), "private fixture")
			public := "shared/public"
			if ownership == "aliased-absent-indexed" {
				if !caseInsensitiveFixture(t, m.Worktree()) {
					t.Skip("requires case insensitive filesystem")
				}
				public = "SHARED/public"
			}
			blob := gitTest(t, m.Worktree(), "rev-parse", "HEAD:public.txt")
			gitTest(t, m.Worktree(), "update-index", "--add", "--cacheinfo", "100644,"+blob+","+public)
			if ownership == "indexed" {
				writeTest(t, m.source(public), "public fixture")
			}
			if ownership == "HEAD-staged-deletion" {
				gitTest(t, m.Worktree(), "commit", "-m", "Public descendant")
				gitTest(t, m.Worktree(), "rm", "--cached", public)
			}
			noPlanMutation(t, base, func() {
				if _, err := m.PlanTrack("shared/"); err == nil {
					t.Error("planned directory with tracked descendant")
				}
			})
			noMutationExceptLockDirectory(t, m, base, func() {
				if err := m.Track("shared"); err == nil {
					t.Error("imported directory with tracked descendant")
				}
			})
		})
	}
}

func TestMappingDirectoryOverlapRefused(t *testing.T) {
	for _, directoryFirst := range []bool{false, true} {
		m, base := setup(t)
		requireInit(t, m)
		writeTest(t, m.source("shared/nested/file"), "private fixture")
		if directoryFirst {
			requireTrack(t, m, "shared")
		} else {
			requireTrack(t, m, "shared/nested/file")
		}
		candidate := "shared"
		if directoryFirst {
			candidate = "shared/nested"
		}
		noPlanMutation(t, base, func() {
			if _, err := m.PlanTrack(candidate); err == nil {
				t.Error("planned overlapping mapping")
			}
		})
		noMutationExceptLockDirectory(t, m, base, func() {
			if err := m.Track(candidate); err == nil {
				t.Error("imported overlapping mapping")
			}
		})
	}
}

func TestLegacyFileHistoryReadOnlyAndDirectoryMigration(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	writeTest(t, m.source("legacy"), "old file snapshot")
	requireTrack(t, m, "legacy")
	h, err := m.History()
	if err != nil {
		t.Fatal(err)
	}
	first := h[0].ID
	// The old index builder's file-only tree is byte-identical to this layout.
	legacy := configuration{Version: 1, CommonDir: m.common, Paths: []string{"legacy"}}
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(m.root, "config.json"), string(b))
	writeTest(t, m.source("legacy"), "pending edit")
	mkdirTest(t, m.source("shared/empty"))
	noPlanMutation(t, base, func() {
		c, err := m.load()
		if err != nil || c.Version != 1 || c.kind("legacy") != "file" {
			t.Fatal("legacy mapping not readable", err)
		}
		if _, err := m.Status(); err != nil {
			t.Fatal(err)
		}
		if _, err := m.History(); err != nil {
			t.Fatal(err)
		}
		dirtyTest(t, m, true)
		if err := m.PlanRestore("legacy", first); err != nil {
			t.Fatal(err)
		}
		if _, err := m.PlanTrack("shared/"); err != nil {
			t.Fatal(err)
		}
	})
	if _, err = m.Restore("legacy", first); err != nil {
		t.Fatal(err)
	}
	if got := readTest(t, filepath.Join(m.root, "config.json")); got != string(b) {
		t.Fatal("restore rewrote legacy schema")
	}
	requireTrack(t, m, "shared/")
	c, err := m.load()
	if err != nil || c.Version != 2 || c.kind("legacy") != "file" || c.kind("shared") != "directory" {
		t.Fatal("typed migration lost mapping types", err)
	}
	if _, err = m.Restore("legacy", first); err != nil {
		t.Fatal("legacy snapshot no longer restores after migration", err)
	}
	var old struct{ Version int }
	if err = json.Unmarshal([]byte(readTest(t, filepath.Join(m.root, "config.json"))), &old); err != nil || old.Version == 1 {
		t.Fatal("old binaries would accept directory schema")
	}
}

func TestUnknownMappingSchemasFailClosed(t *testing.T) {
	for _, schema := range []string{
		`{"version":3,"mappings":[]}`,
		`{"version":2,"mappings":[{"path":"shared","kind":"unknown"}]}`,
		`{"version":2,"managed_paths":["shared"],"mappings":[]}`,
		`{"version":2,"mappings":[{"path":"shared"}]}`,
	} {
		m, base := setup(t)
		requireInit(t, m)
		var fields map[string]interface{}
		if err := json.Unmarshal([]byte(schema), &fields); err != nil {
			t.Fatal(err)
		}
		fields["source_common_dir"] = m.common
		b, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		writeTest(t, filepath.Join(m.root, "config.json"), string(b))
		noPlanMutation(t, base, func() {
			if _, err := m.Status(); err == nil {
				t.Error("accepted unknown mapping schema")
			}
			if _, err := m.PlanAttach(); err == nil {
				t.Error("planned unknown mapping schema")
			}
		})
	}
}

// Build deliberately unsafe historical objects using plumbing only. They never
// become live filesystem entries and restore must refuse before checkpointing.
func historicalDirectoryTest(t *testing.T, m *Manager, entries string) string {
	t.Helper()
	tree, err := m.git([]byte(entries), "mktree", "-z")
	if err != nil {
		t.Fatal(err)
	}
	files, err := m.git([]byte("040000 tree "+strings.TrimSpace(string(tree))+"\tshared\x00"), "mktree", "-z")
	if err != nil {
		t.Fatal(err)
	}
	root, err := m.git([]byte("040000 tree "+strings.TrimSpace(string(files))+"\tfiles\x00"), "mktree", "-z")
	if err != nil {
		t.Fatal(err)
	}
	commit, err := m.git([]byte("Historical fixture\n"), "commit-tree", strings.TrimSpace(string(root)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(commit))
}

func TestHistoricalUnsafeDirectorySubtreesRefused(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	writeTest(t, m.source("shared/safe"), "live private fixture")
	requireTrack(t, m, "shared")
	object, err := m.git([]byte("historical private fixture"), "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.TrimSpace(string(object))
	head, err := m.git(nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	record := func(mode, typ, id, name string) string { return mode + " " + typ + " " + id + "\t" + name + "\x00" }
	for _, entry := range []string{
		record("120000", "blob", blob, "link"),
		record("160000", "commit", strings.TrimSpace(string(head)), "module"),
		record("100644", "blob", blob, ".git"),
		record("100644", "blob", blob, ".GITIGNORE"),
		record("100644", "blob", blob, "Case") + record("100644", "blob", blob, "case"),
		record("100644", "blob", blob, "Caf\u00e9") + record("100644", "blob", blob, "Cafe\u0301"),
		record("100644", "blob", blob, "bad\nname"),
	} {
		// Nest every unsafe fixture to ensure validation is recursive.
		inner, err := m.git([]byte(entry), "mktree", "-z")
		if err != nil {
			t.Fatal(err)
		}
		revision := historicalDirectoryTest(t, m, record("040000", "tree", strings.TrimSpace(string(inner)), "nested"))
		noPlanMutation(t, base, func() {
			if err := m.PlanRestore("shared/", revision); err == nil {
				t.Error("planned unsafe historical subtree")
			}
		})
		noMutationExceptLockDirectory(t, m, base, func() {
			if _, err := m.Restore("shared", revision); err == nil {
				t.Error("restored unsafe historical subtree")
			}
		})
	}
	revision := historicalDirectoryTest(t, m, record("100644", "blob", blob, "missing"))
	if err = os.Remove(filepath.Join(m.private(), ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	noPlanMutation(t, base, func() {
		if err := m.PlanRestore("shared", revision); err == nil {
			t.Error("planned missing historical descendant blob")
		}
	})
	noMutationExceptLockDirectory(t, m, base, func() {
		if _, err := m.Restore("shared", revision); err == nil {
			t.Error("restored missing historical descendant blob")
		}
	})
}

func TestDirectoryTypeAndContentChangesDirty(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	writeTest(t, m.source("shared/child"), "original private fixture")
	requireTrack(t, m, "shared")
	first, err := m.Snapshot("first")
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, m.source("shared/child"), "changed private fixture")
	noPlanMutation(t, base, func() { dirtyTest(t, m, true) })
	if err = os.Remove(m.source("shared/child")); err != nil {
		t.Fatal(err)
	}
	mkdirTest(t, m.source("shared/child/empty"))
	noPlanMutation(t, base, func() { dirtyTest(t, m, true) })
	if _, err = m.Restore("shared", first); err != nil {
		t.Fatal(err)
	}
	if readTest(t, m.source("shared/child")) != "original private fixture" {
		t.Fatal("type change not restored")
	}
	h, err := m.History()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Restore("shared/", h[1].ID); err != nil {
		t.Fatal("pre-checkpoint directory cannot be restored", err)
	}
	if st, err := os.Stat(m.source("shared/child/empty")); err != nil || !st.IsDir() {
		t.Fatal("pre-checkpoint directory shape lost", err)
	}
	dirtyTest(t, m, false)
}

func TestDirectoryFilesystemAliasOverlapRefused(t *testing.T) {
	for _, pair := range [][2]string{{"Shared", "shared"}, {"Caf\u00e9", "Cafe\u0301"}} {
		for _, ancestor := range []bool{false, true} {
			m, base := setup(t)
			requireInit(t, m)
			writeTest(t, m.source(pair[0]+"/nested/file"), "private fixture")
			a, err := os.Lstat(m.source(pair[0]))
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.Lstat(m.source(pair[1]))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(a, b) {
				continue
			}
			candidate := pair[1] + "/nested"
			if ancestor {
				requireTrack(t, m, pair[0]+"/nested/file")
				candidate = pair[1]
			} else {
				requireTrack(t, m, pair[0])
			}
			noPlanMutation(t, base, func() {
				if _, err := m.PlanTrack(candidate); err == nil {
					t.Error("planned filesystem-aliased directory overlap")
				}
			})
			noMutationExceptLockDirectory(t, m, base, func() {
				if err := m.Track(candidate); err == nil {
					t.Error("imported filesystem-aliased directory overlap")
				}
			})
		}
	}
}

func TestDirectoryTrailingSlashDoesNotCleanUnsafePaths(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	mkdirTest(t, m.source("shared"))
	writeTest(t, m.source("file"), "private fixture")
	noPlanMutation(t, base, func() {
		for _, p := range []string{"../source/shared/", "local/../shared/", "./shared/", "/shared/", "shared/./", ".git/", ".gitignore/", "file/"} {
			if _, err := m.PlanTrack(p); err == nil {
				t.Errorf("normalized unsafe directory path %q", p)
			}
			if err := m.Track(p); err == nil {
				t.Errorf("imported unsafe directory path %q", p)
			}
		}
	})
}

func TestFailedDirectoryImportRetainsOriginalAndCheckpoint(t *testing.T) {
	m, _ := setup(t)
	requireInit(t, m)
	writeTest(t, m.source("shared/nested/file"), "recoverable private fixture")
	mkdirTest(t, m.source("shared/empty"))
	original := filesystemState(t, m.source("shared"))
	exclude := gitTest(t, m.Worktree(), "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	mkdirTest(t, exclude) // forces failure after copy + initial checkpoint
	if err := m.Track("shared/"); err == nil {
		t.Fatal("expected import exclude failure")
	}
	if !reflect.DeepEqual(original, filesystemState(t, m.source("shared"))) {
		t.Fatal("failed import changed original directory")
	}
	c, err := m.load()
	if err != nil || len(c.Paths) != 0 {
		t.Fatal("failed import registered mapping", err)
	}
	live, err := inventory(m.central("shared"), "directory", true)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := m.historical("HEAD", "shared", "directory", true)
	if err != nil || !sameNode(live, checkpoint) {
		t.Fatal("directory recovery checkpoint missing", err)
	}
	if err := m.Track("shared"); err == nil {
		t.Fatal("retry overwrote partial central import")
	}
}

func TestDirectoryAllowlistExcludesPrivateExtras(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	mkdirTest(t, m.source("shared/empty"))
	requireTrack(t, m, "shared")
	writeTest(t, filepath.Join(m.private(), "files", "extra", "private"), "non-managed private fixture")
	writeTest(t, filepath.Join(m.private(), "other", "private"), "non-managed private fixture")
	noPlanMutation(t, base, func() { dirtyTest(t, m, false) })
	writeTest(t, m.source("shared/new"), "managed private fixture")
	if _, err := m.Snapshot("managed changes"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"files/extra", "other"} {
		if _, err := m.git(nil, "cat-file", "-e", "HEAD:"+path); err == nil {
			t.Fatal("non-managed private extras entered history")
		}
	}
}
