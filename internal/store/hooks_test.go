package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func hookSetup(t *testing.T) (*Manager, string, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"} {
		old, ok := os.LookupEnv(k)
		os.Unsetenv(k)
		t.Cleanup(func() {
			if ok {
				os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		})
	}
	m, base := setup(t)
	requireInit(t, m)
	exe := filepath.Join(base, "bin space's", "ygg")
	writeTest(t, exe, "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$HOOK_LOG\"\nexit 19\n")
	if err := os.Chmod(exe, 0755); err != nil {
		t.Fatal(err)
	}
	return m, base, exe
}

func hookTree(t *testing.T, p string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.Walk(p, func(p string, st os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		result[p] = st.Mode().String()
		if st.Mode().IsRegular() {
			result[p] += "|" + readTest(t, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestHooksLifecycle(t *testing.T) {
	for _, mode := range []os.FileMode{0, 0640, 0751} {
		t.Run(mode.String(), func(t *testing.T) {
			m, base, exe := hookSetup(t)
			p, _, backup := m.hookPaths()
			original := "#!/bin/sh\nprintf '%s\\n' \"$PWD\" \"$HOOK_VALUE\" \"$@\" >> \"$ORIGINAL_LOG\"\nexit 23\n"
			if mode != 0 {
				writeTest(t, p, original)
				os.Chmod(p, mode)
			}
			before := hookTree(t, base)
			if _, err := m.InstallHooks(exe, true); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, hookTree(t, base)) {
				t.Fatal("dry install wrote files")
			}
			s, err := m.InstallHooks(exe, false)
			if err != nil || s.State != "installed" {
				t.Fatalf("%+v %v", s, err)
			}
			before = hookTree(t, base)
			if _, err = m.InstallHooks(exe, false); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, hookTree(t, base)) {
				t.Fatal("repeat changed files")
			}
			log := filepath.Join(base, "handler-log")
			origLog := filepath.Join(base, "original-log")
			t.Setenv("HOOK_LOG", log)
			t.Setenv("ORIGINAL_LOG", origLog)
			t.Setenv("HOOK_VALUE", "inherited")
			run := func(flag string) string {
				cmd := exec.Command("/bin/sh", p, "old hash", "new' hash", flag, "extra")
				cmd.Dir = m.worktree
				out, err := cmd.CombinedOutput()
				want := 0
				if mode&0111 != 0 {
					want = 23
				}
				got := 0
				if err != nil {
					if e, ok := err.(*exec.ExitError); ok {
						got = e.ExitCode()
					} else {
						t.Fatal(err)
					}
				}
				if got != want {
					t.Fatalf("exit %d want %d: %s", got, want, out)
				}
				return string(out)
			}
			run("0")
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatal("flag0 called handler")
			}
			if out := run("1"); !strings.Contains(out, "attach failed") {
				t.Fatal(out)
			}
			want := "--data-root\n" + m.dataRoot + "\nhook-post-checkout\n--common-dir\n" + m.common + "\nold hash\nnew' hash\n1\nextra\n"
			if readTest(t, log) != want {
				t.Fatal("pinned arguments incorrect")
			}
			if mode&0111 != 0 {
				want := m.worktree + "\ninherited\nold hash\nnew' hash\n0\nextra\n" + m.worktree + "\ninherited\nold hash\nnew' hash\n1\nextra\n"
				if readTest(t, origLog) != want {
					t.Fatal("original context/args/count incorrect")
				}
			} else if _, err := os.Stat(origLog); !os.IsNotExist(err) {
				t.Fatal("inactive original ran")
			}
			os.Remove(exe)
			if out := run("1"); !strings.Contains(out, "missing") {
				t.Fatal(out)
			}
			before = hookTree(t, base)
			if _, err = m.UninstallHooks(true); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, hookTree(t, base)) {
				t.Fatal("dry uninstall wrote files")
			}
			if err = os.RemoveAll(m.root); err != nil {
				t.Fatal(err)
			}
			if _, err = m.HooksStatus(); err != nil {
				t.Fatal(err)
			}
			if _, err = m.UninstallHooks(false); err != nil {
				t.Fatal(err)
			}
			if mode != 0 {
				b, got, ok, err := hookFile(p)
				if err != nil || !ok || string(b) != original || got != mode {
					t.Fatalf("restore %v %v", got, err)
				}
			} else if _, err = os.Stat(p); !os.IsNotExist(err) {
				t.Fatal("unexpected hook")
			}
			if _, err = os.Stat(backup); !os.IsNotExist(err) {
				t.Fatal("backup retained")
			}
			if _, err = m.UninstallHooks(false); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHooksConfiguration(t *testing.T) {
	for _, kind := range []string{"local", "global", "include", "worktree", "empty", "override", "parameters", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			m, base, exe := hookSetup(t)
			switch kind {
			case "local":
				gitTest(t, m.worktree, "config", "core.hooksPath", "elsewhere")
			case "empty":
				gitTest(t, m.worktree, "config", "core.hooksPath", "")
			case "global":
				p := filepath.Join(base, "global")
				writeTest(t, p, "[core]\n hooksPath = elsewhere\n")
				t.Setenv("GIT_CONFIG_GLOBAL", p)
			case "include":
				p := filepath.Join(base, "included")
				writeTest(t, p, "[core]\n hooksPath = elsewhere\n")
				gitTest(t, m.worktree, "config", "include.path", p)
			case "worktree":
				gitTest(t, m.worktree, "config", "extensions.worktreeConfig", "true")
				gitTest(t, m.worktree, "config", "--worktree", "core.hooksPath", "elsewhere")
			case "override":
				t.Setenv("GIT_CONFIG_COUNT", "0")
			case "parameters":
				t.Setenv("GIT_CONFIG_PARAMETERS", "'core.hooksPath=elsewhere'")
			case "malformed":
				p := filepath.Join(base, "bad-config")
				writeTest(t, p, "[invalid\n")
				t.Setenv("GIT_CONFIG_GLOBAL", p)
			}
			before := hookTree(t, base)
			for _, dry := range []bool{true, false} {
				if _, err := m.InstallHooks(exe, dry); err == nil {
					t.Fatal("accepted config")
				}
			}
			if !reflect.DeepEqual(before, hookTree(t, base)) {
				t.Fatal("refusal mutated files")
			}
		})
	}
}

func TestHooksUnsafeAndOwnership(t *testing.T) {
	for _, kind := range []string{"wrapper", "backup", "manifest", "collision", "symlink", "directory", "manager", "lock", "other-root", "other-exe"} {
		t.Run(kind, func(t *testing.T) {
			m, base, exe := hookSetup(t)
			p, meta, backup := m.hookPaths()
			switch kind {
			case "wrapper", "backup", "manifest", "other-root", "other-exe":
				writeTest(t, p, "#!/bin/sh\nexit 0\n")
				os.Chmod(p, 0755)
				if _, err := m.InstallHooks(exe, false); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "wrapper":
					writeTest(t, p, "changed")
				case "backup":
					writeTest(t, backup, "changed")
				case "manifest":
					os.Chmod(meta, 0644)
				case "other-root":
					var err error
					m, err = New(m.worktree, filepath.Join(base, "other-data"))
					if err != nil {
						t.Fatal(err)
					}
				case "other-exe":
					exe = filepath.Join(base, "other-exe")
					writeTest(t, exe, "#!/bin/sh\n")
					os.Chmod(exe, 0755)
				}
			case "collision":
				writeTest(t, backup, "unexpected")
			case "symlink":
				os.MkdirAll(filepath.Dir(p), 0755)
				if err := os.Symlink(exe, p); err != nil {
					t.Fatal(err)
				}
			case "directory":
				os.MkdirAll(p, 0755)
			case "manager":
				writeTest(t, p, "#!/bin/sh\n# generated by Husky\n")
			case "lock":
				writeTest(t, filepath.Join(m.common, ".ygg-post-checkout.lock"), "")
			}
			before := hookTree(t, base)
			if _, err := m.InstallHooks(exe, false); err == nil {
				t.Fatal("unsafe install accepted")
			}
			if kind == "wrapper" || kind == "backup" || kind == "manifest" || kind == "other-root" {
				if _, err := m.UninstallHooks(false); err == nil {
					t.Fatal("unsafe uninstall accepted")
				}
			}
			if !reflect.DeepEqual(before, hookTree(t, base)) {
				t.Fatal("unsafe operation mutated files")
			}
		})
	}
}

func TestHooksBypassAndAttachIdentity(t *testing.T) {
	m, _, exe := hookSetup(t)
	if _, err := m.InstallHooks(exe, false); err != nil {
		t.Fatal(err)
	}
	gitTest(t, m.worktree, "config", "core.hooksPath", "elsewhere")
	s, err := m.HooksStatus()
	if err != nil || s.State != "bypassed" {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err = m.UninstallHooks(false); err != nil {
		t.Fatal(err)
	}
	if got := gitTest(t, m.worktree, "config", "--get", "core.hooksPath"); got != "elsewhere" {
		t.Fatal(got)
	}
	if err = m.AttachFromHook(filepath.Dir(m.common)); err == nil {
		t.Fatal("wrong identity accepted")
	}
	if err = m.AttachFromHook(m.common); err != nil {
		t.Fatal(err)
	}
}

func TestHooksLinkedWorktreeAndQuotedPaths(t *testing.T) {
	m, base, exe := hookSetup(t)
	linked := filepath.Join(base, "linked tree's")
	gitTest(t, m.worktree, "worktree", "add", "-b", "linked", linked)
	other, err := New(linked, filepath.Join(base, "data root's"))
	if err != nil {
		t.Fatal(err)
	}
	requireInit(t, other)
	if _, err = other.InstallHooks(exe, false); err != nil {
		t.Fatal(err)
	}
	main, err := New(m.worktree, other.dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	s, err := main.HooksStatus()
	if err != nil || s.State != "installed" {
		t.Fatalf("%+v %v", s, err)
	}
	t.Setenv("HOOK_LOG", filepath.Join(base, "quoted-log"))
	cmd := exec.Command("/bin/sh", s.Path, "a", "b", "1")
	cmd.Dir = linked
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	if got := readTest(t, filepath.Join(base, "quoted-log")); !strings.Contains(got, "--data-root\n"+other.dataRoot+"\n") {
		t.Fatal(got)
	}
	if _, err = main.UninstallHooks(false); err != nil {
		t.Fatal(err)
	}
}

func TestHooksSymlinkDirectoryAndRedirects(t *testing.T) {
	m, base, exe := hookSetup(t)
	p, _, _ := m.hookPaths()
	d := filepath.Join(base, "not-hooks")
	if err := os.Mkdir(d, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(d, filepath.Dir(p)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.InstallHooks(exe, false); err == nil {
		t.Fatal("symlinked hooks accepted")
	}
	if err := os.Remove(filepath.Dir(p)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", filepath.Join(base, "bogus"))
	t.Setenv("GIT_WORK_TREE", filepath.Join(base, "bogus"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(base, "bogus"))
	if _, err := m.InstallHooks(exe, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UninstallHooks(false); err != nil {
		t.Fatal(err)
	}
}
