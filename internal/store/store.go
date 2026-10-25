// Package store manages an opt-in private file/directory repository shared by linked worktrees.
// A failed import can leave a checkpoint and an original in Root()/imports, but
// config.json only records imports whose source link was successfully installed.
// Locks are deliberately never broken automatically.
//
// Filesystem checks defend against unexpected existing links, not a hostile
// process concurrently replacing directories. Imports are not crash-transactional:
// on failure inspect imports/, the private checkpoint, and config.json before
// retrying. A partial central copy is never silently overwritten by Track.
// Directory restore stages and retains prior subtrees in Root()/restore-*;
// clients may observe a briefly absent canonical root during the rename swap.
// These recovery trees are outside the live snapshot allowlist and never pruned
// automatically. Directory backups across filesystems fail without deleting source.
// Git control files named .gitignore are unsupported: replacing one with a
// symlink would disable its rules, since Git does not follow symlinked ignores.
package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/text/unicode/norm"
)

type Manager struct {
	worktree, common, root, dataRoot string
}

type FileStatus struct {
	Path  string
	State string
}

// TrackPlan describes an import without reading source file contents or writing.
// A directory mapping shares its entire subtree, including future additions.
type TrackPlan struct {
	Source         string
	Target         string
	Kind           string
	AlreadyManaged bool
}
type Entry struct {
	ID      string
	Message string
}

type configuration struct {
	Version   int       `json:"version"`
	CommonDir string    `json:"source_common_dir"`
	Paths     []string  `json:"managed_paths"`
	Mappings  []mapping `json:"mappings,omitempty"`
}

func (m *Manager) Root() string     { return m.root }
func (m *Manager) Worktree() string { return m.worktree }
func (m *Manager) private() string  { return filepath.Join(m.root, "private") }
func (m *Manager) central(p string) string {
	return filepath.Join(m.private(), "files", filepath.FromSlash(p))
}
func (m *Manager) source(p string) string { return filepath.Join(m.worktree, filepath.FromSlash(p)) }

// realPath resolves existing ancestors as well as symlinks in existing paths.
func realPath(p string) (string, error) {
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	var suffix []string
	for {
		r, err := filepath.EvalSymlinks(p)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				r = filepath.Join(r, suffix[i])
			}
			return filepath.Clean(r), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		if st, statErr := os.Lstat(p); statErr == nil && st.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("cannot resolve dangling path symlink")
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		suffix = append(suffix, filepath.Base(p))
		p = parent
	}
}

func within(p, parent string) bool {
	r, err := filepath.Rel(parent, p)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}
func overlaps(a, b string) bool { return within(a, b) || within(b, a) }

// gitEnvironment deliberately drops every inherited Git control variable.
func gitEnvironment() []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(e), "GIT_") {
			env = append(env, e)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1")
}

// Errors never include Git's stderr: plumbing failures can contain file values.
func gitRun(dir string, input []byte, args ...string) ([]byte, error) {
	base := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "commit.gpgsign=false",
		"-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false",
		"-c", "core.attributesFile=" + os.DevNull, "-c", "core.pager=cat"}
	cmd := exec.Command("git", append(base, args...)...)
	cmd.Dir = dir
	cmd.Env = gitEnvironment()
	cmd.Stdin = bytes.NewReader(input)
	for _, arg := range args {
		if arg == "commit-tree" {
			cmd.Env = append(cmd.Env,
				"GIT_AUTHOR_NAME=Yggdrasil", "GIT_AUTHOR_EMAIL=yggdrasil@localhost",
				"GIT_COMMITTER_NAME=Yggdrasil", "GIT_COMMITTER_EMAIL=yggdrasil@localhost")
			break
		}
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git operation failed: %w", err)
	}
	return out.Bytes(), nil
}
func (m *Manager) git(input []byte, args ...string) ([]byte, error) {
	// Private trees store raw mapping names, independently of the filesystem's
	// Unicode normalization. Do not let macOS Git precompose path arguments
	// when looking up those same raw names in historical trees.
	base := []string{"-c", "core.precomposeunicode=false", "--git-dir=" + filepath.Join(m.private(), ".git"), "--work-tree=" + m.private()}
	return gitRun(m.private(), input, append(base, args...)...)
}

// sourceGit pins source probes to the discovered worktree, even when the common
// config has core.worktree/core.bare settings intended for a different tree.
func (m *Manager) sourceGit(args ...string) ([]byte, error) {
	base := []string{"-c", "core.bare=false", "--work-tree=" + m.worktree}
	return gitRun(m.worktree, nil, append(base, args...)...)
}

// New inspects source Git metadata only; it never initializes a store.
func New(worktree, dataRoot string) (*Manager, error) {
	w, err := realPath(worktree)
	if err != nil {
		return nil, err
	}
	b, err := gitRun(w, nil, "rev-parse", "--is-bare-repository")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(b)) != "false" {
		return nil, errors.New("bare repositories are not supported")
	}
	b, err = gitRun(w, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	w, err = realPath(strings.TrimSuffix(string(b), "\n"))
	if err != nil {
		return nil, err
	}
	b, err = gitRun(w, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	common, err := realPath(strings.TrimSuffix(string(b), "\n"))
	if err != nil {
		return nil, err
	}
	d, err := realPath(dataRoot)
	if err != nil {
		return nil, err
	}
	if overlaps(d, common) {
		return nil, errors.New("data root overlaps source Git metadata")
	}
	b, err = gitRun(w, nil, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	for _, field := range bytes.Split(b, []byte{0}) {
		if !bytes.HasPrefix(field, []byte("worktree ")) {
			continue
		}
		p, err := realPath(string(field[len("worktree "):]))
		if err != nil {
			return nil, err
		}
		if overlaps(d, p) {
			return nil, errors.New("data root overlaps a source worktree")
		}
	}
	id := sha256.Sum256([]byte(common))
	return &Manager{worktree: w, common: common, root: filepath.Join(d, hex.EncodeToString(id[:])), dataRoot: d}, nil
}

func validPath(p string) error {
	if p == "" || filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\r\n\\") {
		return errors.New("invalid relative file path")
	}
	if strings.HasSuffix(p, "/") {
		return errors.New("trailing slash requires a directory mapping")
	}
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." || c == ".." || strings.EqualFold(c, ".git") {
			return errors.New("unsafe path component")
		}
		if strings.EqualFold(c, ".gitignore") {
			return errors.New(".gitignore paths are unsupported: Git does not follow symlinked ignore files")
		}
	}
	if filepath.ToSlash(filepath.Clean(p)) != p {
		return errors.New("path must be clean and repository-relative")
	}
	return nil
}

// checkPath never follows a symlink, including in ancestors of base.
// Missing tails are allowed only when requested. The leaf may be a symlink for
// source link inspection; all other existing nodes must have the expected type.
func checkPath(p string, missing, linkLeaf bool) error {
	p = filepath.Clean(p)
	var nodes []string
	for q := p; ; q = filepath.Dir(q) {
		nodes = append(nodes, q)
		if filepath.Dir(q) == q {
			break
		}
	}
	for i := len(nodes) - 1; i >= 0; i-- {
		st, err := os.Lstat(nodes[i])
		if os.IsNotExist(err) && missing {
			return nil
		}
		if err != nil {
			return err
		}
		if i == 0 && linkLeaf && st.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink traversal refused")
		}
		if i > 0 && !st.IsDir() {
			return errors.New("non-directory ancestor")
		}
	}
	return nil
}

func regular(p string) error {
	if err := checkPath(p, false, false); err != nil {
		return err
	}
	st, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return errors.New("expected regular file, found directory")
	}
	if !st.Mode().IsRegular() {
		return errors.New("expected an individual regular file")
	}
	return nil
}

func privateDirs(p string) error {
	if err := checkPath(p, true, false); err != nil {
		return err
	}
	return os.MkdirAll(p, 0700)
}

func (m *Manager) lock() (func(), error) {
	if err := checkPath(m.root, false, false); err != nil {
		return nil, err
	}
	p := filepath.Join(m.root, "lock")
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("store locked or lock unavailable (never removed automatically): %w", err)
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	return func() { _ = os.Remove(p) }, nil
}

func (m *Manager) load() (configuration, error) {
	var c configuration
	p := filepath.Join(m.root, "config.json")
	if err := regular(p); err != nil {
		return c, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, errors.New("malformed store configuration")
	}
	if (c.Version != 1 && c.Version != 2) || c.CommonDir != m.common {
		return c, errors.New("store version or source identity mismatch")
	}
	if c.Version == 1 {
		if len(c.Mappings) != 0 {
			return c, errors.New("typed mappings in legacy configuration")
		}
	} else {
		if c.Paths != nil {
			return c, errors.New("legacy paths in typed configuration")
		}
		for _, entry := range c.Mappings {
			if entry.Kind != "file" && entry.Kind != "directory" {
				return c, errors.New("unknown mapping kind")
			}
			c.Paths = append(c.Paths, entry.Path)
		}
	}
	seen := map[string]bool{}
	for _, p := range c.Paths {
		if err := validPath(p); err != nil {
			return c, err
		}
		if seen[p] {
			return c, errors.New("duplicate managed path")
		}
		seen[p] = true
		if err := checkPath(m.central(p), true, true); err != nil {
			return c, err
		}
	}
	for i, p := range c.Paths {
		for _, q := range c.Paths[:i] {
			related, err := relatedPaths(filepath.Join(m.private(), "files"), p, q)
			if err != nil {
				return c, err
			}
			if related {
				return c, errors.New("overlapping or filesystem-aliased managed paths")
			}
		}
	}
	if err := checkPath(filepath.Join(m.private(), ".git"), false, false); err != nil {
		return c, err
	}
	if err := checkPath(filepath.Join(m.private(), "files"), false, false); err != nil {
		return c, err
	}
	for _, dir := range []string{m.root, m.private(), filepath.Join(m.private(), "files"), filepath.Join(m.private(), ".git")} {
		st, err := os.Lstat(dir)
		if err != nil || !st.IsDir() {
			return c, errors.New("malformed store directory")
		}
	}
	// A private repository must be self-contained. Do not let tampered metadata
	// redirect object/ref writes to another repository or follow filesystem links.
	gitDir := filepath.Join(m.private(), ".git")
	if err := filepath.WalkDir(gitDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 || (!d.IsDir() && !d.Type().IsRegular()) {
			return errors.New("unsafe private Git metadata")
		}
		r, _ := filepath.Rel(gitDir, p)
		if r == "commondir" || r == filepath.Join("objects", "info", "alternates") || r == filepath.Join("objects", "info", "http-alternates") {
			return errors.New("private Git repository must not use external metadata")
		}
		return nil
	}); err != nil {
		return c, err
	}
	b, err = m.git(nil, "rev-parse", "--absolute-git-dir")
	if err != nil || strings.TrimSpace(string(b)) != filepath.Join(m.private(), ".git") {
		return c, errors.New("malformed private Git repository")
	}
	return c, nil
}

func atomicWrite(p string, b []byte) error {
	if err := checkPath(p, true, false); err != nil {
		return err
	}
	if st, err := os.Lstat(p); err == nil && !st.Mode().IsRegular() {
		return errors.New("refusing non-regular destination")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".ygg-write-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
func (m *Manager) save(c configuration) error {
	sort.Strings(c.Paths)
	// All successful mapping saves migrate to v2. Reads never rewrite v1.
	wire := struct {
		Version   int       `json:"version"`
		CommonDir string    `json:"source_common_dir"`
		Mappings  []mapping `json:"mappings"`
	}{Version: 2, CommonDir: c.CommonDir, Mappings: []mapping{}}
	for _, p := range c.Paths {
		wire.Mappings = append(wire.Mappings, mapping{Path: p, Kind: c.kind(p)})
	}
	b, err := json.MarshalIndent(wire, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(m.root, "config.json"), append(b, '\n'))
}

func (m *Manager) Init() error {
	if err := privateDirs(m.root); err != nil {
		return err
	}
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Lstat(filepath.Join(m.root, "config.json")); err == nil {
		_, err = m.load()
		return err
	} else if !os.IsNotExist(err) {
		return err
	}
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() != "lock" {
			return errors.New("uninitialized store is not empty; inspect partial state before retrying")
		}
	}
	if err = privateDirs(filepath.Join(m.private(), "files")); err != nil {
		return err
	}
	if _, err = gitRun(m.private(), nil, "init", "--template=", "--initial-branch=main", m.private()); err != nil {
		return err
	}
	// Git-created directories inherit umask; explicitly restrict the entire private repository.
	if err = filepath.WalkDir(m.private(), func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return os.Chmod(p, 0700)
		}
		return os.Chmod(p, 0600)
	}); err != nil {
		return err
	}
	return m.save(configuration{Version: 1, CommonDir: m.common, Paths: []string{}})
}

// nestedBoundary rejects any .git marker below the source root without opening
// it or consulting nested repository config. The caller checks ancestors first
// so even a symlinked .git marker is inspected with Lstat, never followed.
func (m *Manager) nestedBoundary(p string) error {
	if err := checkPath(m.source(p), true, true); err != nil {
		return err
	}
	parent := m.worktree
	components := strings.Split(p, "/")
	for _, c := range components[:len(components)-1] {
		parent = filepath.Join(parent, c)
		if _, err := os.Lstat(filepath.Join(parent, ".git")); err == nil {
			return errors.New("path crosses a nested Git repository boundary")
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// directoryMayIgnoreCase probes existing entries only, without a test file or
// trust in core.ignorecase. An empty/missing directory cannot be diagnosed
// read-only: conservatively treat absent case variants as aliases. Do not infer
// sensitivity from its parent, which might be a different mount/casefold domain.
func directoryMayIgnoreCase(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err == nil {
		names := make(map[string]bool, len(entries))
		for _, entry := range entries {
			names[entry.Name()] = true
		}
		for _, entry := range entries {
			name := entry.Name()
			alternate := strings.ToUpper(name)
			if alternate == name {
				alternate = strings.ToLower(name)
			}
			if alternate == name {
				continue
			}
			if names[alternate] {
				return false, nil
			}
			original, e := os.Lstat(filepath.Join(dir, name))
			if e != nil {
				return false, e
			}
			alias, e := os.Lstat(filepath.Join(dir, alternate))
			if os.IsNotExist(e) {
				return false, nil
			}
			if e != nil {
				return false, e
			}
			return os.SameFile(original, alias), nil
		}
	}
	return true, nil
}

// relatedPaths recognizes equal paths and ancestor/descendant ownership across
// actual filesystem aliases, including case/normalization aliases. Lstat each
// component instead of resolving whole indexed paths through possible symlinks.
// When both spellings are absent, canonical-normalization collisions (including
// mixed case/normalization differences) are conservatively treated as aliases
// on every filesystem. Case sensitivity does not establish normalization
// sensitivity: even case-sensitive APFS can alias canonically equivalent names.
func relatedPaths(base, p, q string) (bool, error) {
	a, b := strings.Split(p, "/"), strings.Split(q, "/")
	dir := base
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			left, le := os.Lstat(filepath.Join(dir, a[i]))
			right, re := os.Lstat(filepath.Join(dir, b[i]))
			if le != nil && !os.IsNotExist(le) {
				return false, le
			}
			if re != nil && !os.IsNotExist(re) {
				return false, re
			}
			equivalent := le == nil && re == nil && os.SameFile(left, right)
			if le != nil && re != nil {
				normalA, normalB := norm.NFD.String(a[i]), norm.NFD.String(b[i])
				rawCaseAlias := strings.EqualFold(a[i], b[i])
				if normalA == normalB || (!rawCaseAlias && strings.EqualFold(normalA, normalB)) {
					equivalent = true
				} else if rawCaseAlias {
					var err error
					equivalent, err = directoryMayIgnoreCase(dir)
					if err != nil {
						return false, err
					}
				}
			}
			if !equivalent {
				return false, nil
			}
		}
		dir = filepath.Join(dir, a[i])
	}
	return true, nil
}

func (m *Manager) tracked(p string) (bool, error) {
	if err := m.nestedBoundary(p); err != nil {
		return false, err
	}
	// Linked worktrees have independent indices and HEADs. Inspect both the
	// selected and common/main index and HEAD, including staged deletions in
	// main that may not exist in the selected worktree's older HEAD.
	// Explicit --work-tree prevents core.worktree in the common repository's
	// config (or config.worktree) from changing pathspec interpretation. Request
	// root-relative names and inspect ancestors too. Listing all entries is
	// deliberate: literal pathspecs omit filesystem-equivalent case aliases.
	owned := func(b []byte) (bool, error) {
		for _, name := range bytes.Split(b, []byte{0}) {
			q := string(name)
			if q != "" {
				related, err := relatedPaths(m.worktree, p, q)
				if err != nil {
					return false, err
				}
				if related {
					return true, nil
				}
			}
		}
		return false, nil
	}
	for _, repository := range [][]string{{"--git-dir=" + m.common}, nil} {
		// Keep every HEAD query, including unborn/corrupt-HEAD diagnosis,
		// pinned to the same repository as the corresponding index probe.
		probe := func(args ...string) ([]byte, error) {
			return m.sourceGit(append(repository, args...)...)
		}
		for _, args := range [][]string{
			{"ls-files", "--full-name", "--cached", "-z"},
			{"ls-tree", "--full-tree", "-r", "--name-only", "-z", "HEAD"},
		} {
			b, err := probe(args...)
			if err != nil {
				// An unborn HEAD is legitimate, but an index query failure is not.
				if args[0] == "ls-tree" {
					if _, e := probe("rev-parse", "--verify", "HEAD"); e != nil {
						ref, refErr := probe("symbolic-ref", "-q", "HEAD")
						if refErr == nil {
							_, refErr = probe("show-ref", "--verify", "--quiet", strings.TrimSuffix(string(ref), "\n"))
							var exit *exec.ExitError
							if errors.As(refErr, &exit) && exit.ExitCode() == 1 {
								continue
							}
						}
					}
				}
				return false, err
			}
			if tracked, err := owned(b); err != nil || tracked {
				return tracked, err
			}
		}
	}
	return false, nil
}

func contains(c configuration, p string) bool {
	for _, q := range c.Paths {
		if p == q {
			return true
		}
	}
	return false
}
func (m *Manager) linked(p string) bool {
	target, err := os.Readlink(m.source(p))
	return err == nil && target == m.central(p)
}

// sourcePrecomposes reports Git's macOS path conversion, not filesystem alias
// behavior. On other platforms Git leaves path bytes raw even if this setting
// is true. An unset setting does not enable conversion. Query the selected
// worktree each time, since worktree-specific config can override common config.
func (m *Manager) sourcePrecomposes() (bool, error) {
	if runtime.GOOS != "darwin" {
		return false, nil
	}
	b, err := m.sourceGit("config", "--bool", "--get", "core.precomposeunicode")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(string(b)) == "true", nil
}

// ignore uses Git's worktree-aware path, rather than assuming .git is a directory.
func (m *Manager) ignore(paths []string) error {
	precompose, err := m.sourcePrecomposes()
	if err != nil {
		return err
	}
	b, err := m.sourceGit("rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	p := strings.TrimSuffix(string(b), "\n")
	if !filepath.IsAbs(p) {
		p = filepath.Join(m.worktree, p)
	}
	if err = checkPath(p, true, false); err != nil {
		return err
	}
	if err = privateDirs(filepath.Dir(p)); err != nil {
		return err
	}
	var old []byte
	if _, err = os.Lstat(p); err == nil {
		if err = regular(p); err != nil {
			return err
		}
		old, err = os.ReadFile(p)
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	result := append([]byte(nil), old...)
	for _, q := range paths {
		// Match the name ordinary source Git sees, not the raw private tree
		// name. Emit only that spelling, rather than hiding both variants on
		// normalization-sensitive filesystems. Verification below retains the
		// source's normal config and receives the original mapping path.
		if precompose {
			q = norm.NFC.String(q)
		}
		var line strings.Builder
		line.WriteByte('/')
		for _, r := range q {
			if strings.ContainsRune("\\*?[]!# ", r) {
				line.WriteByte('\\')
			}
			line.WriteRune(r)
		}
		found := false
		for _, l := range bytes.Split(result, []byte{'\n'}) {
			if string(l) == line.String() {
				found = true
				break
			}
		}
		if !found {
			if len(result) > 0 && result[len(result)-1] != '\n' {
				result = append(result, '\n')
			}
			result = append(result, line.String()...)
			result = append(result, '\n')
		}
	}
	if !bytes.Equal(old, result) {
		if err = atomicWrite(p, result); err != nil {
			return err
		}
	}
	for _, q := range paths {
		if _, err = m.sourceGit("check-ignore", "-q", "--no-index", "--", q); err != nil {
			return errors.New("local exclude rule is ineffective or overridden by repository ignore rules")
		}
	}
	return nil
}

// PlanTrack performs the same nonmutating preflight used by Track. A successful
// plan is advisory, not a reservation; Track revalidates while holding the lock.
func (m *Manager) PlanTrack(p string) (TrackPlan, error) {
	var err error
	p, err = m.normalize(p)
	if err != nil {
		return TrackPlan{}, err
	}
	c, err := m.load()
	if err != nil {
		return TrackPlan{}, err
	}
	return m.trackCheck(c, p)
}

func (m *Manager) trackCheck(c configuration, p string) (TrackPlan, error) {
	plan := TrackPlan{Source: m.source(p), Target: m.central(p), Kind: c.kind(p)}
	tracked, err := m.tracked(p)
	if err != nil {
		return TrackPlan{}, err
	}
	if tracked {
		return TrackPlan{}, errors.New("source path is Git tracked")
	}
	if err = checkPath(m.source(p), false, true); err != nil {
		return TrackPlan{}, err
	}
	if contains(c, p) {
		if m.linked(p) {
			plan.AlreadyManaged = true
			return plan, validateNode(m.central(p), c.kind(p))
		}
		return TrackPlan{}, errors.New("managed source path conflicts with expected link")
	}
	for _, q := range c.Paths {
		for _, base := range []string{m.worktree, filepath.Join(m.private(), "files")} {
			related, e := relatedPaths(base, p, q)
			if e != nil {
				return TrackPlan{}, e
			}
			if related {
				return TrackPlan{}, errors.New("overlapping or filesystem-aliased managed path")
			}
		}
	}
	st, err := os.Lstat(m.source(p))
	if err != nil {
		return TrackPlan{}, err
	}
	if st.IsDir() {
		plan.Kind = "directory"
	}
	if err = validateNode(m.source(p), plan.Kind); err != nil {
		return TrackPlan{}, err
	}
	if err = checkPath(m.central(p), true, false); err != nil {
		return TrackPlan{}, err
	}
	if _, err = os.Lstat(m.central(p)); !os.IsNotExist(err) {
		return TrackPlan{}, errors.New("central destination already exists; inspect partial import")
	}
	return plan, nil
}

func (m *Manager) Track(p string) error {
	var err error
	p, err = m.normalize(p)
	if err != nil {
		return err
	}
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	c, err := m.load()
	if err != nil {
		return err
	}
	plan, err := m.trackCheck(c, p)
	if err != nil {
		return err
	}
	if plan.AlreadyManaged {
		return nil
	}
	if plan.Kind == "directory" {
		return m.trackDirectory(c, p)
	}
	data, err := os.ReadFile(m.source(p))
	if err != nil {
		return err
	}
	if err = privateDirs(filepath.Dir(m.central(p))); err != nil {
		return err
	}
	f, err := os.OpenFile(m.central(p), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	copyData, err := os.ReadFile(m.central(p))
	if err != nil {
		return err
	}
	if !bytes.Equal(data, copyData) {
		return errors.New("import verification failed; original unchanged")
	}
	next := c
	next.Paths = append(append([]string(nil), c.Paths...), p)
	if _, err = m.snapshot(next, "Import "+p); err != nil {
		return fmt.Errorf("import checkpoint failed; original unchanged: %w", err)
	}
	if err = m.ignore([]string{p}); err != nil {
		return err
	}
	// Move, rather than unlink, the original. The backup survives all subsequent failures.
	imports := filepath.Join(m.root, "imports")
	if err = privateDirs(imports); err != nil {
		return err
	}
	backup, err := os.MkdirTemp(imports, "import-")
	if err != nil {
		return err
	}
	backup = filepath.Join(backup, filepath.FromSlash(p))
	if err = privateDirs(filepath.Dir(backup)); err != nil {
		return err
	}
	if err = regular(m.source(p)); err != nil {
		return err
	}
	current, err := os.ReadFile(m.source(p))
	if err != nil {
		return err
	}
	if !bytes.Equal(current, data) {
		return errors.New("source changed during import; original unchanged")
	}
	if tracked, err := m.tracked(p); err != nil {
		return err
	} else if tracked {
		return errors.New("source became Git tracked")
	}
	if err = os.Rename(m.source(p), backup); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return errors.New("cannot preserve original outside worktree; original not replaced")
		}
		// Across filesystems, copy and verify a durable backup before unlinking.
		if err = atomicWrite(backup, data); err != nil {
			return err
		}
		preserved, e := os.ReadFile(backup)
		if e != nil || !bytes.Equal(preserved, data) {
			return errors.New("original backup verification failed; source unchanged")
		}
		if err = regular(m.source(p)); err != nil {
			return err
		}
		current, err = os.ReadFile(m.source(p))
		if err != nil || !bytes.Equal(current, data) {
			return errors.New("source changed during backup; source unchanged")
		}
		if err = os.Remove(m.source(p)); err != nil {
			return err
		}
	}
	if err = os.Chmod(backup, 0600); err != nil {
		return fmt.Errorf("original retained under imports; permission update failed: %w", err)
	}
	if err = os.Symlink(m.central(p), m.source(p)); err != nil {
		return fmt.Errorf("original retained under imports; link installation failed: %w", err)
	}
	if err = m.save(next); err != nil {
		return fmt.Errorf("original retained under imports; link installed but config not updated: %w", err)
	}
	return nil
}

func (m *Manager) attachCheck(c configuration, p string) error {
	tracked, err := m.tracked(p)
	if err != nil {
		return err
	}
	if tracked {
		return errors.New("source path is Git tracked")
	}
	if err = validateNode(m.central(p), c.kind(p)); err != nil {
		return errors.New("central mapping missing or unsafe")
	}
	if err = checkPath(m.source(p), true, true); err != nil {
		return err
	}
	if _, err = os.Lstat(m.source(p)); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if !m.linked(p) {
		return errors.New("source path conflicts with expected link")
	}
	return nil
}

func (m *Manager) Attach() error {
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	c, err := m.load()
	if err != nil {
		return err
	}
	if _, err = m.attachPlan(c); err != nil {
		return err
	}
	if err = m.ignore(c.Paths); err != nil {
		return err
	}
	for _, p := range c.Paths {
		if err = m.attachCheck(c, p); err != nil {
			return err
		}
		if m.linked(p) {
			continue
		}
		if err = privateDirs(filepath.Dir(m.source(p))); err != nil {
			return err
		}
		if err = m.attachCheck(c, p); err != nil {
			return err
		}
		if err = os.Symlink(m.central(p), m.source(p)); err != nil {
			return err
		}
	}
	return nil
}

// PlanAttach validates every configured attachment without creating parents,
// excludes, links, locks, or private history. Only linked/missing states succeed.
func (m *Manager) PlanAttach() ([]FileStatus, error) {
	c, err := m.load()
	if err != nil {
		return nil, err
	}
	return m.attachPlan(c)
}

func (m *Manager) attachPlan(c configuration) ([]FileStatus, error) {
	result := make([]FileStatus, 0, len(c.Paths))
	for i, p := range c.Paths {
		// A selected worktree can have different case/normalization behavior
		// from the store and from the tree that originally imported the roots.
		for _, q := range c.Paths[:i] {
			related, err := relatedPaths(m.worktree, p, q)
			if err != nil {
				return nil, err
			}
			if related {
				return nil, errors.New("overlapping or filesystem-aliased attachment roots")
			}
		}
		if err := m.attachCheck(c, p); err != nil {
			return nil, fmt.Errorf("attach %q: %w", p, err)
		}
		state := "missing"
		if m.linked(p) {
			state = "linked"
		}
		result = append(result, FileStatus{Path: p, State: state})
	}
	return result, nil
}

func (m *Manager) Status() ([]FileStatus, error) {
	c, err := m.load()
	if err != nil {
		return nil, err
	}
	result := make([]FileStatus, 0, len(c.Paths))
	for _, p := range c.Paths {
		s := "linked"
		t, err := m.tracked(p)
		if err != nil {
			return nil, err
		}
		switch {
		case t:
			s = "tracked"
		case validateNode(m.central(p), c.kind(p)) != nil:
			s = "store-missing"
		case checkPath(m.source(p), true, true) != nil:
			s = "conflict"
		default:
			if _, err := os.Lstat(m.source(p)); os.IsNotExist(err) {
				s = "missing"
			} else if err != nil || !m.linked(p) {
				s = "conflict"
			}
		}
		result = append(result, FileStatus{Path: p, State: s})
	}
	return result, nil
}

// snapshot builds a fresh, explicit tree with plumbing. No filters, attributes,
// external diff programs, hooks, or arbitrary untracked files are consulted.
func (m *Manager) snapshot(c configuration, message string) (string, error) {
	return m.snapshotWithEmpty(c, message, false)
}

func (m *Manager) snapshotWithEmpty(c configuration, message string, force bool) (result string, resultErr error) {
	if strings.ContainsRune(message, 0) {
		return "", errors.New("snapshot message must not contain NUL")
	}
	// Git's object fanout/ref directories otherwise use the user's umask. Do
	// not change process-wide umask: this package may be used concurrently.
	defer func() {
		err := filepath.WalkDir(filepath.Join(m.private(), ".git"), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&os.ModeSymlink != 0 {
				return errors.New("unsafe private Git metadata")
			}
			if d.IsDir() {
				return os.Chmod(p, 0700)
			}
			if !d.Type().IsRegular() {
				return errors.New("unsafe private Git metadata")
			}
			return os.Chmod(p, 0600)
		})
		if resultErr == nil && err != nil {
			resultErr = err
		}
	}()
	treeID, err := m.mappingTree(c)
	if err != nil {
		return "", err
	}
	head, _ := m.git(nil, "rev-parse", "--verify", "HEAD")
	parent := strings.TrimSpace(string(head))
	if parent != "" {
		old, err := m.git(nil, "rev-parse", parent+"^{tree}")
		if err != nil {
			return "", err
		}
		if !force && strings.TrimSpace(string(old)) == treeID {
			return parent, nil
		}
	}
	args := []string{"commit-tree", treeID}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	commit, err := m.git([]byte(message+"\n"), args...)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(commit))
	old := parent
	if old == "" {
		old = strings.Repeat("0", len(id))
	}
	if _, err = m.git(nil, "update-ref", "HEAD", id, old); err != nil {
		return "", err
	}
	return id, nil
}

func (m *Manager) Snapshot(message string) (string, error) {
	unlock, err := m.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	c, err := m.load()
	if err != nil {
		return "", err
	}
	return m.snapshot(c, message)
}

func (m *Manager) History() ([]Entry, error) {
	if _, err := m.load(); err != nil {
		return nil, err
	}
	if _, err := m.git(nil, "rev-parse", "--verify", "HEAD"); err != nil {
		return []Entry{}, nil
	}
	b, err := m.git(nil, "log", "--format=%H%x00%s%x00", "--no-show-signature", "HEAD")
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(b, []byte{0})
	var entries []Entry
	for i := 0; i+1 < len(parts); i += 2 {
		entries = append(entries, Entry{ID: strings.TrimSpace(string(parts[i])), Message: string(parts[i+1])})
	}
	return entries, nil
}

func (m *Manager) Dirty() (bool, error) {
	c, err := m.load()
	if err != nil {
		return false, err
	}
	if _, err = m.git(nil, "rev-parse", "--verify", "HEAD"); err != nil {
		return len(c.Paths) > 0, nil
	}
	for _, p := range c.Paths {
		live, err := inventory(m.central(p), c.kind(p), true)
		if err != nil {
			return false, err
		}
		old, err := m.historical("HEAD", p, c.kind(p), true)
		if err != nil {
			if errors.Is(err, errHistoricalAbsent) {
				return true, nil
			}
			return false, err
		}
		if !sameNode(live, old) {
			return true, nil
		}
	}
	return false, nil
}

var revisionPattern = regexp.MustCompile(`^[0-9a-fA-F]{4,64}$`)

// PlanRestore checks the mapping and historical file metadata, without reading
// the historical blob, checkpointing, or changing the live canonical file.
func (m *Manager) PlanRestore(p, revision string) error {
	var err error
	p, err = m.normalize(p)
	if err != nil {
		return err
	}
	_, _, err = m.restoreCheck(p, revision)
	return err
}

func (m *Manager) restoreCheck(p, revision string) (configuration, string, error) {
	if err := validPath(p); err != nil {
		return configuration{}, "", err
	}
	if !revisionPattern.MatchString(revision) {
		return configuration{}, "", errors.New("revision must be a hexadecimal commit ID or prefix")
	}
	c, err := m.load()
	if err != nil {
		return c, "", err
	}
	if !contains(c, p) {
		return c, "", errors.New("file is not managed")
	}
	if err = m.attachCheck(c, p); err != nil {
		return c, "", err
	}
	// Resolve object names, not refs: a branch called "deadbeef" must not
	// override an actual object prefix. Annotated-tag object IDs are not commits.
	b, err := m.git(nil, "rev-parse", "--disambiguate="+strings.ToLower(revision))
	objects := strings.Fields(string(b))
	if err != nil || len(objects) != 1 {
		return c, "", errors.New("revision is not an unambiguous commit")
	}
	id := objects[0]
	kind, err := m.git(nil, "cat-file", "-t", id)
	if err != nil || strings.TrimSpace(string(kind)) != "commit" {
		return c, "", errors.New("revision is not a commit object")
	}
	if _, err = m.historical(id, p, c.kind(p), false); err != nil {
		return c, "", err
	}
	return c, id, nil
}

func (m *Manager) Restore(p, revision string) (string, error) {
	var err error
	p, err = m.normalize(p)
	if err != nil {
		return "", err
	}
	unlock, err := m.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	c, id, err := m.restoreCheck(p, revision)
	if err != nil {
		return "", err
	}
	if c.kind(p) == "directory" {
		return m.restoreDirectory(c, p, id)
	}
	content, err := m.git(nil, "show", id+":files/"+p)
	if err != nil {
		return "", errors.New("managed file absent in revision")
	}
	if _, err = m.snapshot(c, "Checkpoint before restore"); err != nil {
		return "", err
	}
	if err = regular(m.central(p)); err != nil {
		return "", err
	}
	if err = atomicWrite(m.central(p), content); err != nil {
		return "", err
	}
	result, err := m.snapshotWithEmpty(c, "Restore "+p+" from "+id, true)
	if err != nil {
		return result, fmt.Errorf("file restored but post-restore snapshot failed: %w", err)
	}
	return result, nil
}
