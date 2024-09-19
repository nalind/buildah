package reshuffle

import (
	"archive/tar"
	"path"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/containers/storage/pkg/archive"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/slices"
)

func TestApplyDiff(t *testing.T) {
	type layerDiffEntry struct {
		path     string
		typeFlag byte
		target   string
	}
	type testCase struct {
		description   string
		first, second []layerDiffEntry
		results       []string
		linkGroups    [][]string
	}
	generateLayerDiff := func(entries []layerDiffEntry, prefix string) *diffInfo {
		var diffInfo diffInfo
		generateLocation := func(entry layerDiffEntry) *pathLocation {
			typeFlag := byte(tar.TypeReg)
			if entry.typeFlag != 0 {
				typeFlag = entry.typeFlag
			}
			if strings.HasSuffix(entry.path, "/") {
				typeFlag = tar.TypeDir
			}
			return newPathLocation(&tar.Header{
				Name:     prefix + entry.path,
				Typeflag: typeFlag,
				Linkname: entry.target,
			}, &diffInfo, 0, "")
		}
		for _, entry := range entries {
			diffInfo.paths = append(diffInfo.paths, generateLocation(entry))
		}
		return &diffInfo
	}
	remove := func(what string) string {
		dir, base := path.Split(what)
		return dir + archive.WhiteoutPrefix + base
	}
	opaque := func(what string) string {
		joiner := ""
		if !strings.HasSuffix(what, "/") {
			joiner = "/"
		}
		return what + joiner + archive.WhiteoutOpaqueDir
	}
	testCases := []testCase{
		{
			description: "basic",
			first: []layerDiffEntry{
				{path: "a"},
				{path: "b"},
				{path: "c"},
			},
			second: []layerDiffEntry{
				{path: "a"},
				{path: "b"},
				{path: "c"},
			},
			results: []string{"a", "b", "c"},
		},
		{
			description: "add-and-replace",
			first: []layerDiffEntry{
				{path: "a"},
				{path: "b"},
				{path: "c"},
			},
			second: []layerDiffEntry{
				{path: "c"},
				{path: "d"},
				{path: "e"},
			},
			results: []string{"a", "b", "c", "d", "e"},
		},
		{
			description: "removeone",
			first: []layerDiffEntry{
				{path: "a"},
				{path: "b"},
				{path: "c"},
			},
			second: []layerDiffEntry{
				{path: "a"},
				{path: remove("b")},
			},
			results: []string{"a", "c"},
		},
		{
			description: "subdir",
			first: []layerDiffEntry{
				{path: "a/"},
				{path: "a/b"},
				{path: "a/c"},
				{path: "d"},
				{path: "e"},
			},
			results: []string{"a/", "a/b", "a/c", "d", "e"},
		},
		{
			description: "opaque",
			first: []layerDiffEntry{
				{path: "a/"},
				{path: "a/b"},
				{path: "a/c"},
				{path: "d"},
				{path: "e"},
			},
			second: []layerDiffEntry{
				// normally we'd expect a `{path: "a/"}` here
				{path: opaque("a")},
				{path: "f"},
			},
			results: []string{"a/", "d", "e", "f"},
		},
	}
	for _, prefix := range []string{"", "/", "./"} {
		desc := strings.ReplaceAll(strings.ReplaceAll(prefix, "/", "slash"), ".", "dot")
		if desc == "" {
			desc = "none"
		}
		t.Run(desc, func(t *testing.T) {
			for _, testCase := range testCases {
				var tree tree
				t.Run(testCase.description, func(t *testing.T) {
					if prefix != "" {
						require.NoError(t, tree.applyDiff(generateLayerDiff([]layerDiffEntry{{path: prefix}}, "")))
					}
					require.NoError(t, tree.applyDiff(generateLayerDiff(testCase.first, prefix)))
					require.NoError(t, tree.applyDiff(generateLayerDiff(testCase.second, prefix)))
					paths, infos, err := tree.paths()
					require.NoError(t, err, "listing paths in test case tree")
					for _, p := range paths {
						info := infos[p]
						assert.Equal(t, path.Clean(p), info.Name())
						pl, ok := info.(*pathLocation)
						require.True(t, ok, "type assertion on returned FileInfo")
						hdr := pl.header("")
						require.Equal(t, pl.IsDir(), hdr.Typeflag == tar.TypeDir)
						require.Equal(t, pl.Size(), hdr.Size)
						require.Equal(t, pl.ModTime(), hdr.ModTime)
						require.Equal(t, int64(pl.Mode()&0o777), int64(hdr.Mode&0o777))
					}
					nodes, linkGroups, err := tree.entries()
					require.NoError(t, err, "listing link groups in test case tree")
					assert.Equal(t, len(paths), len(nodes))
					expected := slices.Clone(testCase.results)
					sort.Strings(expected)
					for i := range expected {
						expected[i] = prefix + expected[i]
					}
					if prefix != "" {
						expected = append([]string{prefix}, expected...)
					}
					assert.Equal(t, expected, paths, "list of items in rootfs")
					for i := range testCase.linkGroups {
						expectedGroup := slices.Clone(testCase.linkGroups[i])
						for i := range expectedGroup {
							expectedGroup[i] = prefix + expectedGroup[i]
						}
						sort.Strings(expectedGroup)
						matched := false
						for _, linkGroup := range linkGroups {
							group := slices.Clone(linkGroup)
							sort.Strings(group)
							for i := range group {
								group[i] = prefix + group[i]
							}
							if reflect.DeepEqual(expectedGroup, group) {
								matched = true
							}
						}
						if !matched {
							t.Errorf("no link group found that matched %v", expectedGroup)
						}
					}
				})
			}
		})
	}
}
