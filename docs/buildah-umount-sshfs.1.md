# buildah-umount-sshfs "1" "June 2026" "buildah"

## NAME
buildah\-umount\-sshfs - Unmount a filesystem which was mounted by **buildah mount-sshfs**.

## SYNOPSIS
**buildah umount-sshfs** [*container*|*image* ...]

## DESCRIPTION
Unmounts the specified container or image's root file system, provided it was
mounted using **buildah-mount-sshfs**.

## OPTIONS

**--all**, **-a**
Attempt to unmount all currently-mounted filesystems mounted at locations that
**buildah mount-sshfs** would use as a mountpoint.

## RETURN VALUE
On error an empty string and errno is returned.

## EXAMPLE

```
buildah mount-sshfs working-container
/var/lib/containers/storage/overlay-containers/f3ac502d97b5681989dff84dfedc8354239bcecbdc2692f9a639f4e080a02364/userdata/buildah-shfs-mount
```

```
buildah umount-sshfs working-container
```

## SEE ALSO
buildah(1), buildah-mount-sshfs(1), buildah-unshare(1), buildah-umount(1)
