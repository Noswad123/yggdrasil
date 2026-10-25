package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

type mapping struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

func (c configuration) kind(p string) string {
	for _, entry := range c.Mappings {
		if entry.Path == p {
			return entry.Kind
		}
	}
	return "file" // v1 paths are always individual files
}

// normalize removes trailing separators ONLY after validating the literal root.
// A configured directory may already be a root link, but arbitrary links are
// still refused by trackCheck/attachCheck. Never Clean user input here.
func (m *Manager) normalize(p string) (string, error) {
	if !strings.HasSuffix(p, "/") {
		return p, validPath(p)
	}
	root := strings.TrimRight(p, "/")
	if err := validPath(root); err != nil {
		return "", err
	}
	if err := checkPath(m.source(root), true, true); err != nil {
		return "", err
	}
	st, err := os.Lstat(m.source(root))
	if err == nil && st.IsDir() {
		return root, nil
	}
	c, loadErr := m.load()
	if loadErr == nil && contains(c, root) && c.kind(root) == "directory" {
		return root, nil
	}
	return "", errors.New("trailing slash requires a directory mapping")
}

// node is an explicit tree: a non-nil children map denotes a directory,
// including an empty one. Blobs are always raw, without filters or attributes.
type node struct {
	children map[string]*node
	data     []byte
}

func dirNode() *node { return &node{children: map[string]*node{}} }

func validateNode(root, kind string) error {
	_, err := inventory(root, kind, false)
	return err
}

// Conservatively refuse portable aliases within a shared tree. Historical trees
// can have spellings that collide only when materialized on the current volume.
func safeNames(names []string) error {
	var seen []string
	for _, name := range names {
		if err := validPath(name); err != nil {
			return err
		}
		if strings.Contains(name, "/") {
			return errors.New("invalid tree entry name")
		}
		key := norm.NFD.String(name)
		for _, previous := range seen {
			if strings.EqualFold(key, previous) {
				return errors.New("filesystem-aliased tree entries")
			}
		}
		seen = append(seen, key)
	}
	return nil
}

func inventory(root, kind string, contents bool) (*node, error) {
	if err := checkPath(root, false, false); err != nil {
		return nil, err
	}
	st, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if kind == "file" {
		if !st.Mode().IsRegular() {
			return nil, errors.New("expected regular file")
		}
		n := &node{}
		if contents {
			n.data, err = os.ReadFile(root)
		}
		return n, err
	}
	if kind != "directory" || !st.IsDir() {
		return nil, errors.New("expected directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if err = safeNames(names); err != nil {
		return nil, err
	}
	n := dirNode()
	for _, entry := range entries {
		childKind := "file"
		if entry.IsDir() {
			childKind = "directory"
		}
		child, err := inventory(filepath.Join(root, entry.Name()), childKind, contents)
		if err != nil {
			return nil, err
		}
		n.children[entry.Name()] = child
	}
	return n, nil
}

func sameNode(a, b *node) bool {
	if (a.children == nil) != (b.children == nil) || !bytes.Equal(a.data, b.data) || len(a.children) != len(b.children) {
		return false
	}
	for name, child := range a.children {
		other, ok := b.children[name]
		if !ok || !sameNode(child, other) {
			return false
		}
	}
	return true
}

func nodeNames(n *node) []string {
	names := make([]string, 0, len(n.children))
	for name := range n.children {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// materialize creates a new root exclusively; it never merges or overwrites.
// Failure leaves a recovery/staging tree for inspection rather than deleting it.
func materialize(root string, n *node) error {
	if n.children != nil {
		if err := os.Mkdir(root, 0700); err != nil {
			return err
		}
		for _, name := range nodeNames(n) {
			if err := materialize(filepath.Join(root, name), n.children[name]); err != nil {
				return err
			}
		}
		return nil
	}
	f, err := os.OpenFile(root, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(n.data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (m *Manager) trackDirectory(c configuration, p string) error {
	original, err := inventory(m.source(p), "directory", true)
	if err != nil {
		return err
	}
	if err = privateDirs(filepath.Dir(m.central(p))); err != nil {
		return err
	}
	if err = materialize(m.central(p), original); err != nil {
		return err
	}
	copy, err := inventory(m.central(p), "directory", true)
	if err != nil || !sameNode(original, copy) {
		return errors.New("directory copy verification failed; original unchanged")
	}
	next := c
	next.Paths = append(append([]string(nil), c.Paths...), p)
	next.Mappings = append(append([]mapping(nil), c.Mappings...), mapping{Path: p, Kind: "directory"})
	if _, err = m.snapshot(next, "Import "+p); err != nil {
		return fmt.Errorf("import checkpoint failed; original unchanged: %w", err)
	}
	if err = m.ignore([]string{p}); err != nil {
		return err
	}
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
	current, err := inventory(m.source(p), "directory", true)
	if err != nil || !sameNode(original, current) {
		return errors.New("source changed during directory import; original unchanged")
	}
	if tracked, err := m.tracked(p); err != nil {
		return err
	} else if tracked {
		return errors.New("source became Git tracked")
	}
	// In particular, EXDEV never triggers recursive deletion of the original.
	// The source and central checkpoint remain recoverable on rename failure.
	if err = os.Rename(m.source(p), backup); err != nil {
		return errors.New("cannot preserve original directory outside worktree; source unchanged; central checkpoint retained")
	}
	// Keep original subtree permissions; only the private copy is restricted.
	if err = os.Symlink(m.central(p), m.source(p)); err != nil {
		return fmt.Errorf("original retained under imports; link installation failed: %w", err)
	}
	if err = m.save(next); err != nil {
		return fmt.Errorf("original retained under imports; link installed but config not updated: %w", err)
	}
	return nil
}

func (m *Manager) restoreDirectory(c configuration, p, id string) (string, error) {
	n, err := m.historical(id, p, "directory", true)
	if err != nil {
		return "", err
	}
	// Siblings of private/, not beneath files/: neither staging nor retained
	// previous roots can leak into subsequent allowlisted snapshots.
	stage, err := os.MkdirTemp(m.root, "restore-")
	if err != nil {
		return "", err
	}
	replacement := filepath.Join(stage, "replacement")
	if err = materialize(replacement, n); err != nil {
		return "", err
	}
	verified, err := inventory(replacement, "directory", true)
	if err != nil || !sameNode(n, verified) {
		return "", errors.New("restore staging verification failed")
	}
	if _, err = m.snapshot(c, "Checkpoint before restore"); err != nil {
		return "", err
	}
	if err = m.attachCheck(c, p); err != nil {
		return "", err
	}
	previous := filepath.Join(stage, "previous")
	if err = os.Rename(m.central(p), previous); err != nil {
		return "", err
	}
	// Clients can observe a brief absent root between these renames. Previous
	// is retained permanently for recovery, including after successful swaps.
	if err = os.Rename(replacement, m.central(p)); err != nil {
		if rollback := os.Rename(previous, m.central(p)); rollback != nil {
			return "", errors.New("restore swap and rollback failed; previous subtree retained in restore staging directory")
		}
		return "", fmt.Errorf("restore swap failed; original restored: %w", err)
	}
	result, err := m.snapshotWithEmpty(c, "Restore "+p+" from "+id, true)
	if err != nil {
		return result, fmt.Errorf("directory restored but post-restore snapshot failed; previous subtree retained: %w", err)
	}
	return result, nil
}
