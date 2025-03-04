# Forced to the latest release version number instead of "latest" at build-time
FROM registry.fedoraproject.org/fedora:latest as build
RUN dnf -y install golang gpgme-devel make libseccomp-devel shadow-utils-subid-devel
ENV GOPATH=/root/go
RUN --mount=type=bind,target=/root/go/src/github.com/containers/buildah,rw make -C /root/go/src/github.com/containers/buildah clean all install

# Forced to a freshly-locally-built version of this image instead of the one in the registry, at build-time
FROM quay.io/build/stable
COPY LICENSE /
COPY --from=build /usr/local/bin/* /usr/local/bin/
LABEL org.opencontainers.image.source ${SOURCE}
LABEL org.opencontainers.image.description "github.com/containers/buildah built from upstream repository\nRun with `--device /dev/fuse`"
