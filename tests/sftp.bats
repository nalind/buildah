#!/usr/bin/env bats

load helpers

@test "sftp-server" {
  skip_if_no_sshfs

  _prefetch busybox

  local mountpoint=${TEST_SCRATCH_DIR}/mountpoint
  mkdir -p ${mountpoint}

  run_buildah from --cidfile ${TEST_SCRATCH_DIR}/cid.txt $WITH_POLICY_JSON busybox
  local cid=$(< ${TEST_SCRATCH_DIR}/cid.txt)

  run_buildah info | tee ${TEST_SCRATCH_DIR}/info.txt

  coproc "${PIPELOOP_BINARY}" "${BUILDAH_BINARY} ${BUILDAH_REGISTRY_OPTS} ${ROOTDIR_OPTS} serve-sftp ${cid}" "sshfs -f -o passive :/ ${mountpoint}"
  waited=0
  while ! test -s ${mountpoint}/bin/id ; do
    if test ${waited} -gt 30 ; then
      kill $COPROC_PID
      return
    fi
    sleep 1
    waited=$(( waited++ ))
  done

  run ${mountpoint}/bin/id -u
  echo "$output"
  assert "$result" -eq 0
  assert "$output" -eq $UID

  umount -l ${mountpoint}
}
