package reshuffle

import (
	"archive/tar"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/containers/storage/pkg/archive"
	"github.com/sirupsen/logrus"
)

// tree keeps track of an item in a rootfs
type tree struct {
	self     *pathLocation
	nlink    int
	children map[string]*tree
}

// traverse walks the tree parents-first, but not necessarily in any
// otherwise-sorted order, and visits every node that has a corresponding tar
// header
func traverse(t *tree, visit func(t *tree, path string)) {
	type traversed struct {
		parent string
		child  string
		t      *tree
	}
	queue := []traversed{{t: t}}
	n := 0
	for len(queue) > 0 {
		n++
		var this traversed
		this, queue = queue[0], queue[1:]
		var name string
		if this.child != "" {
			name = this.child
			if this.parent != "" {
				switch this.parent {
				case "/":
					name = "/" + this.child
				default:
					name = strings.TrimSuffix(this.parent, "/") + "/" + this.child
				}
			}
		} else if this.t.self != nil {
			name = this.t.self.name
		}
		if this.t.self != nil {
			nodeName := name
			if this.t.self.typeFlag == tar.TypeDir && !strings.HasSuffix(nodeName, "/") {
				nodeName += "/"
			}
			visit(this.t, nodeName)
		}
		for child, t := range this.t.children {
			queue = append(queue, traversed{parent: name, child: child, t: t})
		}
	}
}

// paths returns the list of all of the items in the tree which have
// corresponding tar headers
func (t *tree) paths() ([]string, map[string]fs.FileInfo, error) {
	var paths []string
	infos := make(map[string]fs.FileInfo)
	traverse(t, func(t *tree, path string) {
		paths = append(paths, path)
		infos[path] = t.self.withName(path)
	})
	sort.Strings(paths)
	return paths, infos, nil
}

// entries returns a mapping from the list of all of the items in the tree
// which have tar headers to the information we have about them, and a mapping
// from the nodes which have multiple hard links to the names of those links
func (t *tree) entries() (map[string]*tree, map[*tree][]string, error) {
	entries := make(map[string]*tree)
	linkGroups := make(map[*tree][]string)
	traverse(t, func(t *tree, path string) {
		entries[path] = t
		if t.nlink > 1 {
			linkGroups[t] = append(linkGroups[t], path)
		}
	})
	return entries, linkGroups, nil
}

// applyDiff processes the contents of a layer diff and makes changes to what
// the tree has in its rootfs
func (t *tree) applyDiff(diff *diffInfo) error {
	for _, location := range diff.paths {
		if err := t.applyDiffLocation(t, location.name, location); err != nil {
			return err
		}
	}
	return nil
}

// remove removes an item from the tree
func (t *tree) remove(basename string) error {
	if t.children == nil {
		return errors.New("not a directory")
	}
	child, ok := t.children[basename]
	if !ok {
		return fmt.Errorf("%q: no such item", basename)
	}
	child.nlink--
	delete(t.children, basename)
	return nil
}

// applyDiffLocation applies a single entry from a layer diff
func (t *tree) applyDiffLocation(root *tree, path string, location *pathLocation) error {
	first, rest, found := strings.Cut(path, "/")
	if first == ".." {
		return errors.New(`illegal path component ".."`)
	}
	if found {
		// we have a (possibly empty) directory component and a (possibly empty) base name component
		if first == "" || first == "." {
			if err := t.applyDiffLocation(root, rest, location); err != nil {
				return fmt.Errorf("under %q: %w", first, err)
			}
			return nil
		}
		// we have a (not empty) directory component and a (possibly empty) base name component
		if t.children == nil {
			t.children = make(map[string]*tree)
		}
		subtree, ok := t.children[first]
		if !ok {
			subtree = &tree{}
			t.children[first] = subtree
		}
		return subtree.applyDiffLocation(root, rest, location)
	} else {
		// "first" is the final path component
		if first == "" || first == "." {
			t.self = location
			t.nlink = 0
			return nil
		}
		if t.children == nil {
			t.children = make(map[string]*tree)
		}
		t.nlink = 0
		// check for the special-handling prefix
		if strings.HasPrefix(first, archive.WhiteoutPrefix) {
			if first == archive.WhiteoutOpaqueDir {
				// this is a directory that should be made empty
				childNames := make([]string, 0, len(t.children))
				for k := range t.children {
					childNames = append(childNames, k)
				}
				for _, childName := range childNames {
					t.remove(childName)
				}
				return nil
			}
			// remove this item from the directory
			whatToDelete := strings.TrimPrefix(first, archive.WhiteoutPrefix)
			if _, present := t.children[whatToDelete]; !present {
				logrus.Warnf("deleting %q: not present?", whatToDelete)
			}
			return t.remove(strings.TrimPrefix(first, archive.WhiteoutPrefix))
		}
		// resolve hard links
		if location.typeFlag == tar.TypeLink {
			target := root
			component, rest, found := strings.Cut(location.linkTarget, "/")
			for found {
				switch component {
				case "..":
					return fmt.Errorf(`illegal component ".." in link target for %q: %q`, location.name, location.linkTarget)
				case "", ".":
					break
				default:
					if len(target.children) == 0 {
						return fmt.Errorf("hard link to missing component %s in target %s for %s", component, location.linkTarget, location.name)
					}
					if _, ok := target.children[component]; !ok {
						return fmt.Errorf("hard link to missing component %s in target %s for %s", component, location.linkTarget, location.name)
					}
				}
				target = target.children[component]
				component, rest, found = strings.Cut(rest, "/")
			}
			if len(target.children) == 0 {
				return fmt.Errorf("hard link to missing final component %s in target %s for %s", component, location.linkTarget, location.name)
			}
			if _, ok := target.children[component]; !ok {
				return fmt.Errorf("hard link to missing final component %s in target %s for %s", component, location.linkTarget, location.name)
			}
			target.children[component].nlink++
			t.children[first] = target.children[component]
			return nil
		}
		// everything else, just create or overwrite the child with the name, keeping its children in case it's a directory
		previousChild := t.children[first]
		t.children[first] = &tree{
			self:  location,
			nlink: 1,
		}
		if previousChild != nil && previousChild.children != nil {
			t.children[first].children = previousChild.children
		}
		return nil
	}
}
