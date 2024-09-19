// Package reshuffle provides an API for reshuffling the contents of an image's
// layers, replacing its M (M >= 0) layer blobs with N (N >= 0) layer blobs
// which will produce the same final rootfs, but where the layers no longer
// include contents which are either removed or overwritten by "later" layers.
package reshuffle
