package store

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Refused mutations may acquire/remove a lock, changing only the store root's
// directory timestamps/size. All entries, modes, contents and other directory
// metadata must remain identical, including excludes, indices and private refs.
func noMutationExceptLockDirectory(t *testing.T, m *Manager, base string, fn func()) {
	t.Helper()
	before := filesystemState(t, base)
	fn()
	after := filesystemState(t, base)
	root, err := filepath.Rel(base, m.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []map[string]filesystemEntry{before, after} {
		entry := state[root]
		entry.Modified, entry.Size = 0, 0
		state[root] = entry
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("refusal changed fixture outside lock-directory metadata")
	}
}

func TestGitignoreRefusalPreservesOtherIgnoredSecrets(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	writeTest(t, m.source(".gitignore"), "secret.env\n")
	writeTest(t, m.source("secret.env"), "root private fixture")
	writeTest(t, m.source("nested/.gitignore"), "other.env\n")
	writeTest(t, m.source("nested/other.env"), "nested private fixture")
	gitTest(t, m.Worktree(), "check-ignore", "--", "secret.env", "nested/other.env")
	noPlanMutation(t, base, func() {
		for _, p := range []string{".gitignore", ".GITIGNORE", "nested/.gitignore", "nested/.GitIgnore", ".gitignore/child", "nested/.GITIGNORE/child"} {
			if _, err := m.PlanTrack(p); err == nil {
				t.Errorf("planned unsupported ignore path %q", p)
			}
			if err := m.Track(p); err == nil {
				t.Errorf("imported unsupported ignore path %q", p)
			}
		}
	})
	// In particular, the ignore file must not have been replaced by a symlink.
	if err := regular(m.source(".gitignore")); err != nil {
		t.Fatal(err)
	}
	gitTest(t, m.Worktree(), "check-ignore", "--", "secret.env")
	gitTest(t, m.Worktree(), "check-ignore", "--", "nested/other.env")
	if _, err := os.Stat(filepath.Join(m.common, "info")); !os.IsNotExist(err) {
		t.Fatal("refusal created source exclude directory")
	}
}

func TestGitignoreMappingsFailClosed(t *testing.T) {
	for _, p := range []string{".gitignore", "nested/.GITIGNORE", ".gitignore/child"} {
		t.Run(p, func(t *testing.T) {
			m, base := setup(t)
			requireInit(t, m)
			// Simulate an unsafe mapping left by an older implementation.
			if err := m.save(configuration{Version: 1, CommonDir: m.common, Paths: []string{p}}); err != nil {
				t.Fatal(err)
			}
			noPlanMutation(t, base, func() {
				if _, err := m.PlanAttach(); err == nil {
					t.Error("planned unsafe mapping")
				}
				if _, err := m.Status(); err == nil {
					t.Error("reported unsafe mapping as usable")
				}
			})
			noMutationExceptLockDirectory(t, m, base, func() {
				if err := m.Attach(); err == nil {
					t.Error("attached unsafe mapping")
				}
			})
		})
	}
}

func caseInsensitiveFixture(t *testing.T, dir string) bool {
	t.Helper()
	f, err := os.CreateTemp(dir, "YggCaseProbe-")
	if err != nil {
		t.Fatal(err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(name)
	a, err := os.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Lstat(filepath.Join(dir, strings.ToLower(filepath.Base(name))))
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(a, b)
}

func TestCaseAliasTrackedImportRefusedBeforeCopy(t *testing.T) {
	m, base := setup(t)
	if !caseInsensitiveFixture(t, m.Worktree()) {
		t.Skip("requires a case-insensitive source filesystem")
	}
	requireInit(t, m)
	writeTest(t, m.source("Config.env"), "public tracked fixture")
	gitTest(t, m.Worktree(), "add", "Config.env")
	check := func(t *testing.T) {
		t.Helper()
		noPlanMutation(t, base, func() {
			if _, err := m.PlanTrack("config.env"); err == nil {
				t.Error("planned filesystem alias of tracked file")
			}
		})
		noMutationExceptLockDirectory(t, m, base, func() {
			if err := m.Track("config.env"); err == nil {
				t.Error("imported filesystem alias of tracked file")
			}
		})
		if _, err := os.Lstat(m.central("config.env")); !os.IsNotExist(err) {
			t.Fatal("tracked refusal copied contents to store")
		}
		if err := regular(m.source("Config.env")); err != nil {
			t.Fatal("tracked source replaced", err)
		}
		history, err := m.History()
		if err != nil || len(history) != 0 {
			t.Fatal("tracked refusal changed history", err)
		}
	}
	t.Run("index-only", check)
	gitTest(t, m.Worktree(), "commit", "-m", "Public config fixture")
	t.Run("committed", check)
	gitTest(t, m.Worktree(), "rm", "--cached", "Config.env")
	t.Run("staged-deletion", check)
}

func TestCaseAliasOwnershipAcrossAllOperations(t *testing.T) {
	m, base := setup(t)
	if !caseInsensitiveFixture(t, m.Worktree()) {
		t.Skip("requires a case-insensitive source filesystem")
	}
	requireInit(t, m)
	writeTest(t, m.source("config.env"), "private fixture")
	requireTrack(t, m, "config.env")
	h, err := m.History()
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(base, "linked")
	gitTest(t, m.Worktree(), "worktree", "add", "-b", "case-alias", wt)
	m2, err := New(wt, filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	publicBlob := gitTest(t, m.Worktree(), "rev-parse", "HEAD:public.txt")
	gitTest(t, m.Worktree(), "update-index", "--add", "--cacheinfo", "100644,"+publicBlob+",Config.env")
	for _, manager := range []*Manager{m, m2} {
		// m has an existing aliased link. m2 has neither spelling on disk,
		// but the common index's ownership must still prevent attachment.
		noPlanMutation(t, base, func() {
			if _, err := manager.PlanTrack("config.env"); err == nil {
				t.Error("planned tracked alias")
			}
			if _, err := manager.PlanAttach(); err == nil {
				t.Error("planned attachment over tracked alias")
			}
			if err := manager.PlanRestore("config.env", h[0].ID); err == nil {
				t.Error("planned restore over tracked alias")
			}
			s, err := manager.Status()
			if err != nil || len(s) != 1 || s[0].State != "tracked" {
				t.Errorf("tracked alias status: %+v %v", s, err)
			}
		})
		noMutationExceptLockDirectory(t, manager, base, func() {
			if err := manager.Track("config.env"); err == nil {
				t.Error("imported tracked alias")
			}
			if err := manager.Attach(); err == nil {
				t.Error("attached over tracked alias")
			}
			if _, err := manager.Restore("config.env", h[0].ID); err == nil {
				t.Error("restored over tracked alias")
			}
		})
	}
}

func TestFilesystemAliasesOfTrackedNamesAndAncestors(t *testing.T) {
	for _, kind := range []string{"gitlink-ancestor", "unicode-normalization"} {
		t.Run(kind, func(t *testing.T) {
			m, base := setup(t)
			requireInit(t, m)
			var indexed, candidate, alternate, mode, object string
			if kind == "gitlink-ancestor" {
				indexed, candidate, alternate, mode = "Module", "module/private.env", "module", "160000"
				object = gitTest(t, m.Worktree(), "rev-parse", "HEAD")
				writeTest(t, m.source("Module/private.env"), "private fixture")
			} else {
				indexed, candidate, alternate, mode = "Caf\u00e9.env", "Cafe\u0301.env", "Cafe\u0301.env", "100644"
				object = gitTest(t, m.Worktree(), "rev-parse", "HEAD:public.txt")
				writeTest(t, m.source(indexed), "public fixture")
			}
			a, err := os.Lstat(m.source(indexed))
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.Lstat(m.source(alternate))
			if os.IsNotExist(err) {
				t.Skip("filesystem does not alias these spellings")
			}
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(a, b) {
				t.Skip("filesystem keeps these spellings distinct")
			}
			gitTest(t, m.Worktree(), "update-index", "--add", "--cacheinfo", mode+","+object+","+indexed)
			noPlanMutation(t, base, func() {
				if _, err := m.PlanTrack(candidate); err == nil {
					t.Error("planned import across aliased tracked ownership")
				}
			})
			noMutationExceptLockDirectory(t, m, base, func() {
				if err := m.Track(candidate); err == nil {
					t.Error("imported across aliased tracked ownership")
				}
			})
		})
	}
}

func TestManagedCaseAliasesDoNotOverwrite(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	if !caseInsensitiveFixture(t, filepath.Join(m.private(), "files")) {
		t.Skip("requires a case-insensitive store filesystem")
	}
	writeTest(t, m.source("Config.env"), "private fixture")
	requireTrack(t, m, "Config.env")
	// On a case-sensitive source / insensitive store, this is a distinct file;
	// on an insensitive source it is already the original managed link.
	if _, err := os.Lstat(m.source("config.env")); os.IsNotExist(err) {
		writeTest(t, m.source("config.env"), "distinct fixture")
	}
	noPlanMutation(t, base, func() {
		if _, err := m.PlanTrack("config.env"); err == nil {
			t.Error("planned case alias of central destination")
		}
	})
	noMutationExceptLockDirectory(t, m, base, func() {
		if err := m.Track("config.env"); err == nil {
			t.Error("overwrote case alias of central destination")
		}
	})
	if err := m.save(configuration{Version: 1, CommonDir: m.common, Paths: []string{"Config.env", "config.env"}}); err != nil {
		t.Fatal(err)
	}
	noPlanMutation(t, base, func() {
		if _, err := m.PlanAttach(); err == nil {
			t.Error("accepted filesystem-aliased mappings")
		}
		if _, err := m.Status(); err == nil {
			t.Error("accepted filesystem-aliased mappings")
		}
	})
}

func TestCaseDistinctFilesRemainSupported(t *testing.T) {
	m, base := setup(t)
	if caseInsensitiveFixture(t, m.Worktree()) {
		t.Skip("requires a case-sensitive source filesystem")
	}
	requireInit(t, m)
	writeTest(t, m.source("Config.env"), "public fixture")
	gitTest(t, m.Worktree(), "add", "Config.env")
	gitTest(t, m.Worktree(), "commit", "-m", "Public config fixture")
	gitTest(t, m.Worktree(), "config", "core.ignorecase", "true")
	writeTest(t, m.source("config.env"), "distinct private fixture")
	noPlanMutation(t, base, func() {
		if _, err := m.PlanTrack("config.env"); err != nil {
			t.Error("case-distinct file incorrectly refused", err)
		}
	})
	requireTrack(t, m, "config.env")
	if readTest(t, m.source("Config.env")) != "public fixture" {
		t.Fatal("changed distinct tracked file")
	}
	wt := filepath.Join(base, "linked")
	gitTest(t, m.Worktree(), "worktree", "add", "-b", "case-distinct", wt)
	m2, err := New(wt, filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	noPlanMutation(t, base, func() {
		if _, err := m2.PlanAttach(); err != nil {
			t.Error("case-distinct missing destination refused", err)
		}
	})
	if err := m2.Attach(); err != nil {
		t.Fatal(err)
	}
}

func TestMissingCaseVariantsInEmptyDirectoryFailClosed(t *testing.T) {
	base, err := realPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	noPlanMutation(t, base, func() {
		related, err := relatedPaths(base, "Config.env", "config.env")
		if err != nil || !related {
			t.Error("unknown empty-directory case behavior must fail closed", err)
		}
	})
}

func TestMissingNormalizationAliasInLinkedIndexRefusesAttach(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	// Keep Git's command-line paths decomposed during import on macOS too.
	gitTest(t, m.Worktree(), "config", "core.precomposeunicode", "false")
	managed, indexed := "Cafe\u0301.env", "Caf\u00e9.env"
	writeTest(t, m.source(managed), "private normalization fixture")
	requireTrack(t, m, managed)
	wt := filepath.Join(base, "normalization-linked")
	gitTest(t, m.Worktree(), "worktree", "add", "-b", "normalization-alias", wt)
	m2, err := New(wt, filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{managed, indexed} {
		if _, err := os.Lstat(m2.source(p)); !os.IsNotExist(err) {
			t.Fatalf("expected absent spelling %q: %v", p, err)
		}
	}
	publicBlob := gitTest(t, wt, "rev-parse", "HEAD:public.txt")
	gitTest(t, wt, "update-index", "--add", "--cacheinfo", "100644,"+publicBlob+","+indexed)
	index, err := m2.sourceGit("ls-files", "--cached", "-z")
	if err != nil || !strings.Contains(string(index), indexed+"\x00") {
		t.Fatal("fixture must contain composed index-only name", err)
	}
	// Remove the import's exclusion so reaching ignore() would be observable.
	exclude, err := m2.sourceGit("rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, strings.TrimSuffix(string(exclude), "\n"), "# Must remain unchanged on refusal.\n")
	noPlanMutation(t, base, func() {
		if _, err := m2.PlanAttach(); err == nil {
			t.Error("planned attachment over absent normalization alias")
		}
		status, err := m2.Status()
		if err != nil || len(status) != 1 || status[0].State != "tracked" {
			t.Errorf("normalization alias status: %+v %v", status, err)
		}
	})
	noMutationExceptLockDirectory(t, m2, base, func() {
		if err := m2.Attach(); err == nil {
			t.Error("attached over absent normalization alias")
		}
	})
	for _, p := range []string{managed, indexed} {
		if _, err := os.Lstat(m2.source(p)); !os.IsNotExist(err) {
			t.Fatalf("refusal installed a source path %q: %v", p, err)
		}
	}
}

func TestAbsentCanonicalNormalizationCollisionsFailClosed(t *testing.T) {
	base, err := realPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Establish case-sensitive behavior where available. This must not be used
	// to infer normalization sensitivity; canonical collisions still fail closed.
	writeTest(t, filepath.Join(base, "CaseProbe"), "fixture")
	for _, pair := range [][2]string{
		{"Cafe\u0301.env", "Caf\u00e9.env"},
		{"Cafe\u0301.env", "CAF\u00c9.env"},
		{"a\u0315\u0300.env", "a\u0300\u0315.env"},
		{"\uac00.env", "\u1100\u1161.env"},
	} {
		noPlanMutation(t, base, func() {
			related, err := relatedPaths(base, pair[0], pair[1])
			if err != nil || !related {
				t.Errorf("absent canonical collision not protected for %q and %q: %v", pair[0], pair[1], err)
			}
		})
	}
	// Canonical normalization is not compatibility normalization.
	if related, err := relatedPaths(base, "\uff21.env", "A.env"); err != nil || related {
		t.Error("compatibility-only names treated as canonical aliases", err)
	}
}

func TestExistingNormalizationSpellingsUseFilesystemIdentity(t *testing.T) {
	base, err := realPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	composed, decomposed := "Caf\u00e9.env", "Cafe\u0301.env"
	writeTest(t, filepath.Join(base, composed), "fixture")
	writeTest(t, filepath.Join(base, decomposed), "fixture")
	a, err := os.Lstat(filepath.Join(base, composed))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Lstat(filepath.Join(base, decomposed))
	if err != nil {
		t.Fatal(err)
	}
	noPlanMutation(t, base, func() {
		related, err := relatedPaths(base, composed, decomposed)
		if err != nil || related != os.SameFile(a, b) {
			t.Error("existing filesystem identity must override normalization fallback", err)
		}
	})
}

func TestIndependentNestedRepositoryImportRefused(t *testing.T) {
	m, base := setup(t)
	requireInit(t, m)
	nested := m.source("nested")
	gitTest(t, m.Worktree(), "init", "--template=", "--initial-branch=main", nested)
	writeTest(t, filepath.Join(nested, "config.env"), "nested tracked fixture")
	gitTest(t, nested, "add", "config.env")
	gitTest(t, nested, "commit", "-m", "Nested public fixture")
	noPlanMutation(t, base, func() {
		if _, err := m.PlanTrack("nested/config.env"); err == nil {
			t.Error("planned import across independent repo")
		}
	})
	noMutationExceptLockDirectory(t, m, base, func() {
		if err := m.Track("nested/config.env"); err == nil {
			t.Error("imported nested tracked file")
		}
	})
}

func TestNestedBoundariesRefuseExistingMappings(t *testing.T) {
	for _, kind := range []string{"directory", "file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			m, base := setup(t)
			requireInit(t, m)
			p := "nested/deeper/file"
			writeTest(t, m.source(p), "private fixture")
			requireTrack(t, m, p)
			h, err := m.History()
			if err != nil {
				t.Fatal(err)
			}
			marker := m.source("nested/.git")
			switch kind {
			case "directory":
				err = os.Mkdir(marker, 0700)
			case "file":
				writeTest(t, marker, "gitdir: nonexistent-destination\n")
			case "symlink":
				err = os.Symlink(m.common, marker)
			}
			if err != nil {
				t.Fatal(err)
			}
			noPlanMutation(t, base, func() {
				if _, err := m.PlanTrack(p); err == nil {
					t.Error("planned nested mapping")
				}
				if _, err := m.PlanAttach(); err == nil {
					t.Error("planned nested attachment")
				}
				if _, err := m.Status(); err == nil {
					t.Error("nested mapping status did not fail closed")
				}
				if err := m.PlanRestore(p, h[0].ID); err == nil {
					t.Error("planned nested restore")
				}
			})
			noMutationExceptLockDirectory(t, m, base, func() {
				if err := m.Track(p); err == nil {
					t.Error("imported nested mapping")
				}
				if err := m.Attach(); err == nil {
					t.Error("attached nested mapping")
				}
				if _, err := m.Restore(p, h[0].ID); err == nil {
					t.Error("restored nested mapping")
				}
			})
		})
	}
}
