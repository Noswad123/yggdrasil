package store

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type filesystemEntry struct {
	Mode     os.FileMode
	Modified int64
	Size     int64
	Digest   [32]byte
	Link     string
}

// Capture the whole artificial fixture, including directories, excludes, source
// indices, config, private objects/refs, permissions and modification timestamps.
// Never follow symlinks or print file values on a comparison failure.
func filesystemState(t *testing.T, root string) map[string]filesystemEntry {
	t.Helper()
	state := map[string]filesystemEntry{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		entry := filesystemEntry{Mode: info.Mode(), Modified: info.ModTime().UnixNano(), Size: info.Size()}
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			entry.Digest = sha256.Sum256(b)
		} else if info.Mode()&os.ModeSymlink != 0 {
			entry.Link, err = os.Readlink(p)
			if err != nil {
				return err
			}
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		state[rel] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func noPlanMutation(t *testing.T, root string, fn func()) {
	t.Helper()
	before := filesystemState(t, root)
	fn()
	after := filesystemState(t, root)
	if !reflect.DeepEqual(before, after) {
		for p, old := range before {
			if current, ok := after[p]; !ok || current != old {
				t.Errorf("plan changed or removed %q", p)
			}
		}
		for p := range after {
			if _, ok := before[p]; !ok {
				t.Errorf("plan created %q", p)
			}
		}
	}
}

func TestPlansWithoutInitializedStoreCreateNothing(t *testing.T) {
	m, base := setup(t)
	writeTest(t, m.source("file"), "private fixture")
	noPlanMutation(t, base, func() {
		if _, err := m.PlanTrack("file"); err == nil {
			t.Error("planned uninitialized import")
		}
		if _, err := m.PlanAttach(); err == nil {
			t.Error("planned uninitialized attach")
		}
		if err := m.PlanRestore("file", "1234"); err == nil {
			t.Error("planned uninitialized restore")
		}
	})
	if _, err := os.Lstat(filepath.Dir(m.Root())); !os.IsNotExist(err) {
		t.Fatal("plan created data root")
	}
}

func TestSuccessfulPlansAreReadOnly(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	p := "local/nested/file"
	writeTest(t, m.source(p), "first private fixture")
	// Empty-template source repositories do not yet have info/ or exclude.
	if _, err := os.Lstat(filepath.Join(m.common, "info")); !os.IsNotExist(err) {
		t.Fatal("test needs missing source info directory")
	}
	noPlanMutation(t, base, func() {
		plan, err := m.PlanTrack(p)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Source != m.source(p) || plan.Target != m.central(p) || plan.AlreadyManaged {
			t.Fatalf("incorrect import plan: %+v", plan)
		}
	})
	requireTrack(t, m, p)
	h, err := m.History()
	if err != nil {
		t.Fatal(err)
	}
	first := h[0].ID
	writeTest(t, m.source(p), "unsnapshotted newer fixture")
	wt := filepath.Join(base, "linked")
	gitTest(t, m.Worktree(), "worktree", "add", "-b", "preview", wt)
	m2, err := New(wt, filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	// Plans must neither acquire nor remove the existing mutation lock.
	unlock, err := m.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	noPlanMutation(t, base, func() {
		plan, err := m.PlanTrack(p)
		if err != nil || !plan.AlreadyManaged {
			t.Fatalf("managed plan: %+v %v", plan, err)
		}
		linked, err := m.PlanAttach()
		if err != nil || len(linked) != 1 || linked[0] != (FileStatus{Path: p, State: "linked"}) {
			t.Fatalf("linked plan: %+v %v", linked, err)
		}
		missing, err := m2.PlanAttach()
		if err != nil || len(missing) != 1 || missing[0] != (FileStatus{Path: p, State: "missing"}) {
			t.Fatalf("missing plan: %+v %v", missing, err)
		}
		if err = m.PlanRestore(p, first[:12]); err != nil {
			t.Fatal(err)
		}
		if err = m2.PlanRestore(p, first); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Lstat(m2.source("local")); !os.IsNotExist(err) {
		t.Fatal("attach preview created parent directory")
	}
}

func TestPlanRefusalsAreReadOnly(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	writeTest(t, m.source("file"), "private fixture")
	requireTrack(t, m, "file")
	h, err := m.History()
	if err != nil {
		t.Fatal(err)
	}
	first := h[0].ID
	writeTest(t, m.source("later"), "later fixture")
	requireTrack(t, m, "later")
	writeTest(t, m.source("orphan"), "original fixture")
	writeTest(t, m.central("orphan"), "partial copy fixture")
	if err = os.Symlink(m.source("public.txt"), m.source("alias")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(m.Worktree(), m.source("ancestor")); err != nil {
		t.Fatal(err)
	}
	noPlanMutation(t, base, func() {
		for _, p := range []string{"../file", "sub/../file", "/file", ".git/config", "a\nfile", "missing", "public.txt", "alias", "ancestor/public.txt", "orphan"} {
			if _, err := m.PlanTrack(p); err == nil {
				t.Errorf("accepted import %q", p)
			}
		}
		for _, rev := range []string{"HEAD", "--help", first + "~1", "123", "ffffffffff"} {
			if err := m.PlanRestore("file", rev); err == nil {
				t.Errorf("accepted revision %q", rev)
			}
		}
		if err := m.PlanRestore("unknown", first); err == nil {
			t.Error("accepted unmanaged file")
		}
		if err := m.PlanRestore("later", first); err == nil {
			t.Error("accepted missing historical file")
		}
		if err := m.PlanRestore("../file", first); err == nil {
			t.Error("accepted traversal")
		}
	})
	// A conflict late in the allowlist must refuse the whole attach plan.
	if err = os.Remove(m.source("later")); err != nil {
		t.Fatal(err)
	}
	writeTest(t, m.source("later"), "do not clobber")
	noPlanMutation(t, base, func() {
		if _, err := m.PlanTrack("later"); err == nil {
			t.Error("accepted managed regular-file conflict")
		}
		if _, err := m.PlanAttach(); err == nil {
			t.Error("accepted attach conflict")
		}
		if err := m.PlanRestore("later", first); err == nil {
			t.Error("accepted restore destination conflict")
		}
	})
	gitTest(t, m.Worktree(), "add", "-f", "later")
	noPlanMutation(t, base, func() {
		if _, err := m.PlanTrack("later"); err == nil {
			t.Error("accepted tracked import")
		}
		if _, err := m.PlanAttach(); err == nil {
			t.Error("accepted tracked attachment")
		}
		if err := m.PlanRestore("later", first); err == nil {
			t.Error("accepted tracked restore")
		}
	})
	if err = os.Remove(m.central("file")); err != nil {
		t.Fatal(err)
	}
	noPlanMutation(t, base, func() {
		if _, err := m.PlanTrack("file"); err == nil {
			t.Error("accepted missing central file")
		}
		if _, err := m.PlanAttach(); err == nil {
			t.Error("accepted missing central attachment")
		}
		if err := m.PlanRestore("file", first); err == nil {
			t.Error("accepted missing central restore")
		}
	})
	writeTest(t, filepath.Join(m.Root(), "config.json"), "not json")
	noPlanMutation(t, base, func() {
		if _, err := m.PlanTrack("public.txt"); err == nil {
			t.Error("accepted malformed configuration")
		}
		if _, err := m.PlanAttach(); err == nil {
			t.Error("accepted malformed configuration")
		}
		if err := m.PlanRestore("file", first); err == nil {
			t.Error("accepted malformed configuration")
		}
	})
}

func TestPlansWithUnbornAndCorruptSourceHEAD(t *testing.T) {
	base, err := realPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(base, "source")
	gitTest(t, base, "init", "--template=", "--initial-branch=main", src)
	m, err := New(src, filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	requireInit(t, m)
	writeTest(t, m.source("file"), "private fixture")
	noPlanMutation(t, base, func() {
		if _, err := m.PlanTrack("file"); err != nil {
			t.Error("unborn source HEAD rejected", err)
		}
	})
	writeTest(t, filepath.Join(m.common, "HEAD"), "not a HEAD\n")
	noPlanMutation(t, base, func() {
		if _, err := m.PlanTrack("file"); err == nil {
			t.Error("corrupt source HEAD accepted")
		}
	})
}

func TestRestorePlanRejectsHistoricalSymlinkAndMissingBlob(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	writeTest(t, m.source("file"), "historical fixture")
	requireTrack(t, m, "file")
	h, err := m.History()
	if err != nil {
		t.Fatal(err)
	}
	object, err := m.git(nil, "rev-parse", h[0].ID+":files/file")
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.TrimSpace(string(object))
	inner, err := m.git([]byte("120000 blob "+blob+"\tfile\x00"), "mktree", "-z")
	if err != nil {
		t.Fatal(err)
	}
	root, err := m.git([]byte("040000 tree "+strings.TrimSpace(string(inner))+"\tfiles\x00"), "mktree", "-z")
	if err != nil {
		t.Fatal(err)
	}
	commit, err := m.git([]byte("Historical symlink fixture\n"), "commit-tree", strings.TrimSpace(string(root)))
	if err != nil {
		t.Fatal(err)
	}
	noPlanMutation(t, base, func() {
		if err := m.PlanRestore("file", strings.TrimSpace(string(commit))); err == nil {
			t.Error("accepted historical symlink")
		}
		if err := m.PlanRestore("file", blob); err == nil {
			t.Error("accepted non-commit revision")
		}
	})
	if err := os.Remove(filepath.Join(m.private(), ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	noPlanMutation(t, base, func() {
		if err := m.PlanRestore("file", h[0].ID); err == nil {
			t.Error("accepted missing historical blob")
		}
	})
}

func TestMutationsRevalidateAfterPlans(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	writeTest(t, m.source("new"), "private fixture")
	if _, err := m.PlanTrack("new"); err != nil {
		t.Fatal(err)
	}
	gitTest(t, m.Worktree(), "add", "new")
	if err := m.Track("new"); err == nil {
		t.Fatal("Track trusted stale plan")
	}
	if err := regular(m.source("new")); err != nil {
		t.Fatal("Track replaced stale source", err)
	}
	writeTest(t, m.source("file"), "first")
	requireTrack(t, m, "file")
	h, err := m.History()
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(base, "linked")
	gitTest(t, m.Worktree(), "worktree", "add", "-b", "stale", wt)
	m2, err := New(wt, filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m2.PlanAttach(); err != nil {
		t.Fatal(err)
	}
	writeTest(t, m2.source("file"), "conflict")
	if err = m2.Attach(); err == nil {
		t.Fatal("Attach trusted stale plan")
	}
	if readTest(t, m2.source("file")) != "conflict" {
		t.Fatal("Attach overwrote conflict")
	}
	writeTest(t, m.source("file"), "pending edit")
	if err = m.PlanRestore("file", h[0].ID); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(m.source("file")); err != nil {
		t.Fatal(err)
	}
	writeTest(t, m.source("file"), "conflict")
	if _, err = m.Restore("file", h[0].ID); err == nil {
		t.Fatal("Restore trusted stale plan")
	}
	current, err := m.History()
	if err != nil || !reflect.DeepEqual(h, current) {
		t.Fatal("failed stale restore created a checkpoint")
	}
	if readTest(t, m.central("file")) != "pending edit" {
		t.Fatal("failed stale restore changed canonical file")
	}
}

func TestTrackedAncestorsAndPinnedCommonIndex(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	wt := filepath.Join(base, "linked")
	gitTest(t, m.Worktree(), "worktree", "add", "-b", "ownership", wt)
	m2, err := New(wt, filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	head := gitTest(t, m.Worktree(), "rev-parse", "HEAD")
	gitTest(t, m.Worktree(), "update-index", "--add", "--cacheinfo", "160000,"+head+",owned")
	writeTest(t, m.source("owned/private"), "private fixture")
	writeTest(t, m2.source("owned/private"), "private fixture")
	writeTest(t, m.source("folder/public"), "public fixture")
	gitTest(t, m.Worktree(), "add", "folder/public")
	writeTest(t, m.source("folder/private"), "private fixture")
	noPlanMutation(t, base, func() {
		if _, err := m.PlanTrack("owned/private"); err == nil {
			t.Error("accepted tracked gitlink ancestor")
		}
		if _, err := m2.PlanTrack("owned/private"); err == nil {
			t.Error("ignored common-index gitlink ancestor")
		}
		if _, err := m.PlanTrack("folder/private"); err != nil {
			t.Error("tracked sibling incorrectly blocks import", err)
		}
	})
	gitTest(t, m.Worktree(), "commit", "-m", "Track gitlink ancestor")
	gitTest(t, m.Worktree(), "rm", "--cached", "owned")
	noPlanMutation(t, base, func() {
		if _, err := m.PlanTrack("owned/private"); err == nil {
			t.Error("ignored HEAD gitlink ancestor after staged removal")
		}
	})
	// This config used to make common-index probes run relative to an unrelated
	// worktree. Both managers were discovered before the deliberate tampering.
	gitTest(t, m.Worktree(), "config", "core.worktree", filepath.Join(base, "nonexistent-configured-tree"))
	noPlanMutation(t, base, func() {
		if _, err := m2.PlanTrack("public.txt"); err == nil {
			t.Error("config redirected common-index tracked probe")
		}
		if _, err := m.PlanTrack("folder/private"); err != nil {
			t.Error("config redirected current-tree ownership probe", err)
		}
	})
	requireTrack(t, m, "folder/private")
}
