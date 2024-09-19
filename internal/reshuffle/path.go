package reshuffle

import (
	"archive/tar"
	"io/fs"
	"path"
	"strings"
	"time"

	digest "github.com/opencontainers/go-digest"
	"golang.org/x/exp/maps"
)

const (
	// See http://pubs.opengroup.org/onlinepubs/9699919799/utilities/pax.html#tag_20_92_13_06, from archive/tar
	cISUID = 0o4000 // Set uid, from archive/tar
	cISGID = 0o2000 // Set gid, from archive/tar
	cISVTX = 0o1000 // Save text (sticky bit), from archive/tar
)

// a pathLocation represents an item in an input layer and contains the
// information we'll need in order to write it to an output layer
type pathLocation struct {
	name                    string
	linkTarget              string
	typeFlag                byte
	size                    int64
	mode                    int64
	uid, gid                int
	mtime, ctime, atime     time.Time
	devMajor, devMinor      int64
	xattrs, paxRecords      map[string]string
	layerDiffInfo           *diffInfo
	layerUncompressedOffset int64
	digest                  digest.Digest // only set when typeFlag == TypeReg
}

func newPathLocation(hdr *tar.Header, layerDiffInfo *diffInfo, offset int64, digest digest.Digest) *pathLocation {
	name := hdr.Name
	if hdr.Typeflag == tar.TypeDir {
		// force directories to always have a terminating /
		if !strings.HasSuffix(hdr.Name, "/") {
			name += "/"
		}
	} else {
		// strip a terminating / off of non-directories, just in case
		if hdr.Name != "" && strings.HasSuffix(hdr.Name, "/") {
			name = strings.TrimSuffix(name, "/")
		}
	}
	return &pathLocation{
		name:                    name,
		linkTarget:              hdr.Linkname,
		typeFlag:                hdr.Typeflag,
		size:                    hdr.Size,
		mode:                    hdr.Mode,
		uid:                     hdr.Uid,
		gid:                     hdr.Gid,
		mtime:                   hdr.ModTime,
		ctime:                   hdr.ChangeTime,
		atime:                   hdr.AccessTime,
		devMajor:                hdr.Devmajor,
		devMinor:                hdr.Devminor,
		xattrs:                  maps.Clone(hdr.Xattrs),
		paxRecords:              maps.Clone(hdr.PAXRecords),
		layerDiffInfo:           layerDiffInfo,
		layerUncompressedOffset: offset,
		digest:                  digest,
	}
}

func (p pathLocation) withName(name string) *pathLocation {
	updated := p
	updated.name = name
	return &updated
}

func (p *pathLocation) header(name string) tar.Header {
	if name == "" {
		name = p.name
	}
	return tar.Header{
		Name:       name,
		Linkname:   p.linkTarget,
		Typeflag:   p.typeFlag,
		Size:       p.size,
		Mode:       p.mode,
		Uid:        p.uid,
		Gid:        p.gid,
		ModTime:    p.mtime,
		ChangeTime: p.ctime,
		AccessTime: p.atime,
		Devmajor:   p.devMajor,
		Devminor:   p.devMinor,
		Xattrs:     maps.Clone(p.xattrs),
		PAXRecords: maps.Clone(p.paxRecords),
	}
}

func (p *pathLocation) Name() string {
	return path.Clean(p.name)
}

func (p *pathLocation) IsDir() bool {
	return p.typeFlag == tar.TypeDir
}

func (p *pathLocation) Size() int64 {
	if p.typeFlag != tar.TypeReg {
		return 0
	}
	return p.size
}

func (p *pathLocation) Sys() any {
	return p
}

func (p *pathLocation) ModTime() time.Time {
	return p.mtime
}

func (p *pathLocation) Mode() fs.FileMode {
	mode := fs.FileMode(p.mode) & fs.ModePerm
	switch p.typeFlag {
	case tar.TypeReg:
	case tar.TypeDir:
		mode |= fs.ModeDir
	case tar.TypeBlock:
		mode |= fs.ModeDevice
	case tar.TypeChar:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case tar.TypeFifo:
		mode |= fs.ModeNamedPipe
	case tar.TypeSymlink:
		mode |= fs.ModeSymlink
	}
	if mode&cISUID == cISUID {
		mode |= fs.ModeSetuid
	}
	if mode&cISGID == cISGID {
		mode |= fs.ModeSetgid
	}
	if mode&cISVTX == cISVTX {
		mode |= fs.ModeSticky
	}
	return mode
}
