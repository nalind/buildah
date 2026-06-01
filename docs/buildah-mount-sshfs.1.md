# buildah-mount-sshfs "1" "June 2026" "buildah"

## NAME
buildah\-mount\-sshfs - Mount a container's or image's root filesystem using sshfs.

## SYNOPSIS
**buildah mount-sshfs** [options] [*container*|*image* ...]

## DESCRIPTION
Mounts the specified container or image's root file system in a location which
can be accessed from the host, and returns its location.

Running `buildah add`, `buildah copy`, or `buildah run` while the filesystem is
mounted can produce unpredictable results, if attempting to do so does not
produce errors from the start.

Mounting a filesystem using sshfs(1) comes with known compatibility limitations.

## OPTIONS

**-o**, **--options**

Options to pass to sshfs as arguments to its *-o* option.

## RETURN VALUE
The location of the mounted file system.  On error an empty string and errno is
returned.

## EXAMPLE

```
buildah mount-sshfs working-container
/var/lib/containers/storage/overlay-containers/f3ac502d97b5681989dff84dfedc8354239bcecbdc2692f9a639f4e080a02364/userdata/buildah-shfs-mount
```

```
buildah umount-sshfs working-container
```

## SEE ALSO
buildah(1), buildah-mount(1), buildah-umount-sshfs(1), buildah-unshare(1), sshfs(1)
