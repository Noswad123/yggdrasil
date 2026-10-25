package store

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

var errHistoricalAbsent = errors.New("managed root absent in revision")

// mappingTree constructs real empty subtrees with mktree, unlike write-tree
// from an index (which necessarily loses empty directories). Ancestors are
// synthetic; only explicitly managed roots are inventoried.
func (m *Manager) mappingTree(c configuration) (string, error) {
	root := dirNode()
	for _, p := range c.Paths {
		n, err := inventory(m.central(p), c.kind(p), true)
		if err != nil {
			return "", fmt.Errorf("managed central mapping %q missing or unsafe: %w", p, err)
		}
		parts := strings.Split("files/"+p, "/")
		parent := root
		for _, part := range parts[:len(parts)-1] {
			if parent.children[part] == nil {
				parent.children[part] = dirNode()
			}
			parent = parent.children[part]
			if parent.children == nil {
				return "", errors.New("overlapping mapping tree")
			}
		}
		parent.children[parts[len(parts)-1]] = n
	}
	return m.writeNode(root)
}

func (m *Manager) writeNode(n *node) (string, error) {
	if n.children == nil {
		b, err := m.git(n.data, "hash-object", "-w", "--stdin")
		return strings.TrimSpace(string(b)), err
	}
	var input bytes.Buffer
	for _, name := range nodeNames(n) {
		child := n.children[name]
		id, err := m.writeNode(child)
		if err != nil {
			return "", err
		}
		mode, kind := "100644", "blob"
		if child.children != nil {
			mode, kind = "040000", "tree"
		}
		fmt.Fprintf(&input, "%s %s %s\t%s%c", mode, kind, id, name, byte(0))
	}
	b, err := m.git(input.Bytes(), "mktree", "-z")
	return strings.TrimSpace(string(b)), err
}

func (m *Manager) historical(revision, p, kind string, contents bool) (*node, error) {
	entry, err := m.git(nil, "ls-tree", "-z", revision, "--", ":(literal)files/"+p)
	if err != nil {
		return nil, err
	}
	records := bytes.Split(entry, []byte{0})
	if len(records) != 2 || len(records[1]) != 0 {
		return nil, errHistoricalAbsent
	}
	mode, typ, id, _, err := treeRecord(records[0])
	if err != nil {
		return nil, err
	}
	if (kind == "directory" && mode != "040000") || (kind == "file" && mode != "100644" && mode != "100755") {
		return nil, errors.New("historical mapping type mismatch")
	}
	return m.readNode(mode, typ, id, contents)
}

func treeRecord(record []byte) (mode, typ, id, name string, err error) {
	header, tail, ok := bytes.Cut(record, []byte{'\t'})
	fields := strings.Fields(string(header))
	if !ok || len(fields) != 3 {
		err = errors.New("malformed historical tree entry")
		return
	}
	return fields[0], fields[1], fields[2], string(tail), nil
}

func (m *Manager) readNode(mode, typ, id string, contents bool) (*node, error) {
	if !((mode == "040000" && typ == "tree") || ((mode == "100644" || mode == "100755") && typ == "blob")) {
		return nil, errors.New("unsafe historical object mode")
	}
	actual, err := m.git(nil, "cat-file", "-t", id)
	if err != nil || strings.TrimSpace(string(actual)) != typ {
		return nil, errors.New("missing or invalid historical object")
	}
	if typ == "blob" {
		n := &node{}
		if contents {
			n.data, err = m.git(nil, "cat-file", "blob", id)
		}
		return n, err
	}
	b, err := m.git(nil, "ls-tree", "-z", id)
	if err != nil {
		return nil, err
	}
	n := dirNode()
	var names []string
	type entry struct{ mode, typ, id, name string }
	var entries []entry
	for _, record := range bytes.Split(b, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		mode, typ, id, name, err := treeRecord(record)
		if err != nil {
			return nil, err
		}
		names = append(names, name)
		entries = append(entries, entry{mode, typ, id, name})
	}
	if err := safeNames(names); err != nil {
		return nil, err
	}
	for _, entry := range entries {
		child, err := m.readNode(entry.mode, entry.typ, entry.id, contents)
		if err != nil {
			return nil, err
		}
		n.children[entry.name] = child
	}
	return n, nil
}
