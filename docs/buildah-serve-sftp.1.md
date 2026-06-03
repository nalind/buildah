# buildah-serve-sftp "1" "June 2026" "buildah"

## NAME
buildah\-serve\-sftp - Share a container's or image's root filesystem using SFTP.

## SYNOPSIS
**buildah serve-sftp** [options] *container*|*image*

## DESCRIPTION
Shares the specified container or image's root file system using SFTP over stdio.

## OPTIONS

**--read-only**
Set the SFTP service to disallow attempts to write to the mounted rootfs.
Always enabled for images, defaults to false for containers.

## RETURN VALUE
None.  The process serves the rootfs content until it is interrupted or killed.

## EXAMPLE

```
dpipe = buildah serve-sftp working-container = sshfs -o passive -f :/ `mktemp -d`
```

## SEE ALSO
buildah(1), buildah-mount-sshfs(1), buildah-umount-sshfs(1), buildah-unshare(1), sshfs(1)
