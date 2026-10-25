package store

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

func TestDecomposedMappingRootSourceExcludes(t *testing.T) {
	for _, precompose := range []string{"true", "false"} {
		for _, kind := range []string{"file", "directory"} {
			t.Run(precompose+"/"+kind, func(t *testing.T) {
				m, base := setup(t)
				requireInit(t, m)
				gitTest(t, m.Worktree(), "config", "core.precomposeunicode", precompose)
				root := "Cafe\u0301"
				file := root
				if kind == "directory" {
					file += "/nested/Cafe\u0301.txt"
				}
				writeTest(t, m.source(file), "private fixture")
				noPlanMutation(t, base, func() {
					if _, err := m.PlanTrack(root); err != nil {
						t.Fatal(err)
					}
				})
				requireTrack(t, m, root)
				wt := filepath.Join(base, "linked")
				gitTest(t, m.Worktree(), "worktree", "add", "-b", "unicode-excludes", wt)
				m2, err := New(wt, filepath.Join(base, "data"))
				if err != nil {
					t.Fatal(err)
				}
				if err := m2.Attach(); err != nil {
					t.Fatal(err)
				}
				for _, manager := range []*Manager{m, m2} {
					if readTest(t, manager.source(file)) != "private fixture" {
						t.Fatal("linked mapping contents differ")
					}
					// Use ordinary source Git, with no invocation-only Unicode
					// override: these rules must actually protect normal status.
					if got := gitTest(t, manager.Worktree(), "status", "--porcelain", "--untracked-files=all"); got != "" {
						t.Fatal("source worktree not clean")
					}
					gitTest(t, manager.Worktree(), "check-ignore", "-q", "--", root)
					exclude := gitTest(t, manager.Worktree(), "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
					before := readTest(t, exclude)
					pattern := root
					if runtime.GOOS == "darwin" && precompose == "true" {
						pattern = norm.NFC.String(root)
					}
					if before != "/"+pattern+"\n" {
						t.Fatal("exclude does not match source Git path normalization exactly")
					}
					if err := manager.Attach(); err != nil {
						t.Fatal(err)
					}
					requireTrack(t, manager, root)
					if readTest(t, exclude) != before {
						t.Fatal("repeated operations duplicated exclude entries")
					}
					if got := gitTest(t, manager.Worktree(), "config", "--get", "core.precomposeunicode"); got != precompose {
						t.Fatal("source config changed")
					}
				}
				dirtyTest(t, m, false)
				h, err := m.History()
				if err != nil || len(h) != 1 {
					t.Fatal("unexpected import history", err)
				}
				if err := m2.PlanRestore(root, h[0].ID); err != nil {
					t.Fatal("raw private history names no longer queryable", err)
				}
				// Where the filesystem keeps canonical spellings distinct, the
				// other spelling is an unrelated path and must remain visible.
				composed := norm.NFC.String(root)
				if _, err := os.Lstat(m.source(composed)); os.IsNotExist(err) && !(runtime.GOOS == "darwin" && precompose == "true") {
					writeTest(t, m.source(composed), "unrelated fixture")
					out, err := m.sourceGit("check-ignore", "--no-index", "--", composed)
					if err == nil || len(out) != 0 {
						t.Fatal("rule hides unrelated composed sibling")
					}
					if got := gitTest(t, m.Worktree(), "-c", "core.quotePath=false", "status", "--porcelain", "--untracked-files=all"); !strings.Contains(got, composed) {
						t.Fatal("unrelated composed sibling is hidden from status")
					}
				}
			})
		}
	}
}

func TestDecomposedMappingRootHistory(t *testing.T) {
	for _, kind := range []string{"file", "directory"} {
		t.Run(kind, func(t *testing.T) {
			m, base := setup(t)
			requireInit(t, m)
			// Match macOS's private-repository default explicitly, even when
			// running on a platform where Git does not precompose argv paths.
			gitTest(t, m.Worktree(), "config", "core.precomposeunicode", "false")
			gitTest(t, m.private(), "config", "core.precomposeunicode", "true")
			root := "Cafe\u0301"
			file := root
			if kind == "directory" {
				file += "/nested/Cafe\u0301.txt"
				mkdirTest(t, m.source(root+"/empty"))
			}
			writeTest(t, m.source(file), "original private fixture")
			requireTrack(t, m, root)
			history, err := m.History()
			if err != nil || len(history) != 1 {
				t.Fatal("missing import checkpoint", err)
			}
			first := history[0].ID
			noPlanMutation(t, base, func() {
				dirtyTest(t, m, false)
				if err := m.PlanRestore(root, first); err != nil {
					t.Fatal("raw Unicode mapping absent from import checkpoint", err)
				}
			})
			writeTest(t, m.source(file), "new private fixture")
			noPlanMutation(t, base, func() {
				dirtyTest(t, m, true)
				if err := m.PlanRestore(root, first); err != nil {
					t.Fatal(err)
				}
			})
			if _, err := m.Restore(root, first); err != nil {
				t.Fatal(err)
			}
			if readTest(t, m.source(file)) != "original private fixture" {
				t.Fatal("raw Unicode historical contents not restored")
			}
			dirtyTest(t, m, false)
			if got := gitTest(t, m.private(), "config", "--get", "core.precomposeunicode"); got != "true" {
				t.Fatal("private command override changed repository config")
			}
			if got := gitTest(t, m.Worktree(), "config", "--get", "core.precomposeunicode"); got != "false" {
				t.Fatal("private command override changed source config")
			}
		})
	}
}

func TestCommonHEADDescendantAfterStagedDeletionRefusesLinkedImport(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	wt := filepath.Join(base, "older-linked")
	gitTest(t, m.Worktree(), "worktree", "add", "-b", "older-linked", wt)
	m2, err := New(wt, filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	// Only common/main HEAD owns this descendant. Neither index nor the
	// selected linked worktree's older HEAD contains it after staged removal.
	writeTest(t, m.source("shared/public"), "public fixture")
	gitTest(t, m.Worktree(), "add", "shared/public")
	gitTest(t, m.Worktree(), "commit", "-m", "Public descendant")
	gitTest(t, m.Worktree(), "rm", "--cached", "shared/public")
	writeTest(t, m2.source("shared/private"), "private fixture")
	noPlanMutation(t, base, func() {
		if _, err := m2.PlanTrack("shared"); err == nil {
			t.Error("planned import of common HEAD owned subtree")
		}
	})
	noMutationExceptLockDirectory(t, m2, base, func() {
		if err := m2.Track("shared"); err == nil {
			t.Error("imported common HEAD owned subtree")
		}
	})
	if _, err := os.Lstat(m2.central("shared")); !os.IsNotExist(err) {
		t.Fatal("ownership refusal copied source contents")
	}
	if _, err := os.Lstat(filepath.Join(m.common, "info")); !os.IsNotExist(err) {
		t.Fatal("ownership refusal created excludes")
	}
	if st, err := os.Lstat(m2.source("shared")); err != nil || !st.IsDir() {
		t.Fatal("ownership refusal replaced source directory", err)
	}
	h, err := m2.History()
	if err != nil || len(h) != 0 {
		t.Fatal("ownership refusal created private history", err)
	}
}

func TestLinkedImportChecksCommonUnbornVersusCorruptHEAD(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	wt := filepath.Join(base, "linked")
	gitTest(t, m.Worktree(), "worktree", "add", "-b", "common-head-check", wt)
	m2, err := New(wt, filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, m2.source("shared/private"), "private fixture")
	gitTest(t, m.Worktree(), "symbolic-ref", "HEAD", "refs/heads/unborn-main")
	noPlanMutation(t, base, func() {
		if _, err := m2.PlanTrack("shared"); err != nil {
			t.Fatal("unborn common HEAD blocked unrelated import", err)
		}
	})
	writeTest(t, filepath.Join(m.common, "HEAD"), "not a valid HEAD\n")
	noPlanMutation(t, base, func() {
		if _, err := m2.PlanTrack("shared"); err == nil {
			t.Error("selected valid HEAD hid corrupt common HEAD")
		}
	})
}
