package tracker

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/jellydator/go-toml/internal/ast"
)

type keyKind uint8

const (
	invalidKind keyKind = iota
	valueKind
	tableKind
	arrayTableKind
)

func (k keyKind) String() string {
	switch k {
	case invalidKind:
		return "invalid"
	case valueKind:
		return "value"
	case tableKind:
		return "table"
	case arrayTableKind:
		return "array table"
	}
	panic("missing keyKind string mapping")
}

// SeenTracker tracks which keys have been seen with which TOML type to flag
// duplicates and mismatches according to the spec.
//
// Each node in the visited tree is represented by an entry. Each entry has an
// identifier, which is provided by a counter. Entries are stored in the array
// entries. As new nodes are discovered (referenced for the first time in the
// TOML document), entries are created and appended to the array. An entry
// points to its parent using its id.
//
// To find whether a given key (sequence of []byte) has already been visited,
// the entries are linearly searched, looking for one with the right name and
// parent id.
//
// Given that all keys appear in the document after their parent, it is
// guaranteed that all descendants of a node are stored after the node, this
// speeds up the search process.
//
// When encountering [[array tables]], the descendants of that node are removed
// to allow that branch of the tree to be "rediscovered". To maintain the
// invariant above, the deletion process needs to keep the order of entries.
// This results in more copies in that case.
type SeenTracker struct {
	entries    []entry
	currentIdx int
}

var pool sync.Pool

func (s *SeenTracker) reset() {
	// Always contains a root element at index 0.
	s.currentIdx = 0
	if len(s.entries) == 0 {
		s.entries = make([]entry, 1, 2)
	} else {
		s.entries = s.entries[:1]
	}
	s.entries[0].child = -1
	s.entries[0].next = -1
}

type entry struct {
	// Use -1 to indicate no child or no sibling.
	child int
	next  int

	name     []byte
	kind     keyKind
	explicit bool
	kv       bool
}

// Find the index of the child of parentIdx with key k. Returns -1 if
// it does not exist.
func (s *SeenTracker) find(parentIdx int, k []byte) int {
	for i := s.entries[parentIdx].child; i >= 0; i = s.entries[i].next {
		if bytes.Equal(s.entries[i].name, k) {
			return i
		}
	}
	return -1
}

// Remove all descendants of node at position idx.
func (s *SeenTracker) clear(idx int) {
	if idx >= len(s.entries) {
		return
	}

	for i := s.entries[idx].child; i >= 0; {
		next := s.entries[i].next
		n := s.entries[0].next
		s.entries[0].next = i
		s.entries[i].next = n
		s.entries[i].name = nil
		s.clear(i)
		i = next
	}

	s.entries[idx].child = -1
}

func (s *SeenTracker) create(parentIdx int, name []byte, kind keyKind, explicit bool, kv bool) int {
	e := entry{
		child: -1,
		next:  s.entries[parentIdx].child,

		name:     name,
		kind:     kind,
		explicit: explicit,
		kv:       kv,
	}
	var idx int
	if s.entries[0].next >= 0 {
		idx = s.entries[0].next
		s.entries[0].next = s.entries[idx].next
		s.entries[idx] = e
	} else {
		idx = len(s.entries)
		s.entries = append(s.entries, e)
	}

	s.entries[parentIdx].child = idx

	return idx
}

func (s *SeenTracker) setExplicitFlag(parentIdx int) {
	for i := s.entries[parentIdx].child; i >= 0; i = s.entries[i].next {
		if s.entries[i].kv {
			s.entries[i].explicit = true
			s.entries[i].kv = false
		}
		s.setExplicitFlag(i)
	}
}

// CheckExpression takes a top-level node and checks that it does not contain
// keys that have been seen in previous calls, and validates that types are
// consistent.
func (s *SeenTracker) CheckExpression(node *ast.Node) error {
	if s.entries == nil {
		s.reset()
	}
	switch node.Kind {
	case ast.KeyValue:
		return s.checkKeyValue(node)
	case ast.Table:
		return s.checkTable(node)
	case ast.ArrayTable:
		return s.checkArrayTable(node)
	default:
		panic(fmt.Errorf("this should not be a top level node type: %s", node.Kind))
	}
}

func (s *SeenTracker) checkTable(node *ast.Node) error {
	if s.currentIdx >= 0 {
		s.setExplicitFlag(s.currentIdx)
	}

	it := node.Key()

	parentIdx := 0

	// This code is duplicated in checkArrayTable. This is because factoring
	// it in a function requires to copy the iterator, or allocate it to the
	// heap, which is not cheap.
	for it.Next() {
		if it.IsLast() {
			break
		}

		k := it.Node().Data

		idx := s.find(parentIdx, k)

		if idx < 0 {
			idx = s.create(parentIdx, k, tableKind, false, false)
		} else {
			entry := s.entries[idx]
			if entry.kind == valueKind {
				return fmt.Errorf("toml: expected %s to be a table, not a %s", string(k), entry.kind)
			}
		}
		parentIdx = idx
	}

	k := it.Node().Data
	idx := s.find(parentIdx, k)

	if idx >= 0 {
		kind := s.entries[idx].kind
		if kind != tableKind {
			return fmt.Errorf("toml: key %s should be a table, not a %s", string(k), kind)
		}
		if s.entries[idx].explicit {
			return fmt.Errorf("toml: table %s already exists", string(k))
		}
		s.entries[idx].explicit = true
	} else {
		idx = s.create(parentIdx, k, tableKind, true, false)
	}

	s.currentIdx = idx

	return nil
}

func (s *SeenTracker) checkArrayTable(node *ast.Node) error {
	if s.currentIdx >= 0 {
		s.setExplicitFlag(s.currentIdx)
	}

	it := node.Key()

	parentIdx := 0

	for it.Next() {
		if it.IsLast() {
			break
		}

		k := it.Node().Data

		idx := s.find(parentIdx, k)

		if idx < 0 {
			idx = s.create(parentIdx, k, tableKind, false, false)
		} else {
			entry := s.entries[idx]
			if entry.kind == valueKind {
				return fmt.Errorf("toml: expected %s to be a table, not a %s", string(k), entry.kind)
			}
		}

		parentIdx = idx
	}

	k := it.Node().Data
	idx := s.find(parentIdx, k)

	if idx >= 0 {
		kind := s.entries[idx].kind
		if kind != arrayTableKind {
			return fmt.Errorf("toml: key %s already exists as a %s,  but should be an array table", kind, string(k))
		}
		s.clear(idx)
	} else {
		idx = s.create(parentIdx, k, arrayTableKind, true, false)
	}

	s.currentIdx = idx

	return nil
}

func (s *SeenTracker) checkKeyValue(node *ast.Node) error {
	parentIdx := s.currentIdx
	it := node.Key()

	for it.Next() {
		k := it.Node().Data

		idx := s.find(parentIdx, k)

		if idx < 0 {
			idx = s.create(parentIdx, k, tableKind, false, true)
		} else {
			entry := s.entries[idx]
			if it.IsLast() {
				return fmt.Errorf("toml: key %s is already defined", string(k))
			} else if entry.kind != tableKind {
				return fmt.Errorf("toml: expected %s to be a table, not a %s", string(k), entry.kind)
			} else if entry.explicit {
				return fmt.Errorf("toml: cannot redefine table %s that has already been explicitly defined", string(k))
			}
		}

		parentIdx = idx
	}

	s.entries[parentIdx].kind = valueKind

	value := node.Value()

	switch value.Kind {
	case ast.InlineTable:
		return s.checkInlineTable(value)
	case ast.Array:
		return s.checkArray(value)
	}

	return nil
}

func (s *SeenTracker) checkArray(node *ast.Node) error {
	it := node.Children()
	for it.Next() {
		n := it.Node()
		switch n.Kind {
		case ast.InlineTable:
			err := s.checkInlineTable(n)
			if err != nil {
				return err
			}
		case ast.Array:
			err := s.checkArray(n)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *SeenTracker) checkInlineTable(node *ast.Node) error {
	if pool.New == nil {
		pool.New = func() interface{} {
			return &SeenTracker{}
		}
	}

	s = pool.Get().(*SeenTracker)
	s.reset()

	it := node.Children()
	for it.Next() {
		n := it.Node()
		err := s.checkKeyValue(n)
		if err != nil {
			return err
		}
	}

	// As inline tables are self-contained, the tracker does not
	// need to retain the details of what they contain. The
	// keyValue element that creates the inline table is kept to
	// mark the presence of the inline table and prevent
	// redefinition of its keys: check* functions cannot walk into
	// a value.
	pool.Put(s)
	return nil
}
