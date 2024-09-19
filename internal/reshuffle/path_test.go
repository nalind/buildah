package reshuffle

import (
	"archive/tar"
	"fmt"
	"strconv"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
)

func TestNewPathLocation(t *testing.T) {
	for i, typeFlag := range []byte{
		tar.TypeReg,
		tar.TypeLink,
		tar.TypeSymlink,
		tar.TypeChar,
		tar.TypeBlock,
		tar.TypeDir,
		tar.TypeFifo,
	} {
		t.Run(fmt.Sprintf("type=%c", typeFlag), func(t *testing.T) {
			i64 := int64(i)
			diffInfo := &diffInfo{}
			contentDigest := digest.FromString(strconv.Itoa(i))
			name := strconv.Itoa(i)
			hdr := &tar.Header{
				Name:     name,
				Linkname: "pointer-to-" + strconv.Itoa(i),
				Typeflag: typeFlag,
				Size:     i64 * 1024,
				Mode:     0o400 + i64,
				Uid:      i + 0x10000,
				Gid:      i + 0x10000 + 1,
				ModTime:  time.Unix(i64, 0),
				Devmajor: i64 + 16,
				Devminor: i64 + 32,
			}
			pl := newPathLocation(hdr, diffInfo, i64, contentDigest)
			hdr2 := pl.header("")
			if typeFlag == tar.TypeDir {
				name += "/"
			}
			assert.Equal(t, name, hdr2.Name)
			assert.True(t, hdr2.ModTime.Equal(hdr.ModTime))
			switch hdr2.Typeflag {
			case tar.TypeLink, tar.TypeSymlink:
				assert.Equal(t, hdr2.Linkname, hdr.Linkname)
				assert.Equal(t, hdr2.Linkname, hdr.Linkname)
			case tar.TypeChar, tar.TypeBlock:
				assert.Equal(t, hdr2.Devmajor, hdr.Devmajor)
				assert.Equal(t, hdr2.Devminor, hdr.Devminor)
			}
			assert.Equal(t, diffInfo, pl.layerDiffInfo)
			assert.Equal(t, contentDigest, pl.digest)
		})
	}
}
