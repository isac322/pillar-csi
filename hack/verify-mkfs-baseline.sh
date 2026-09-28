#!/bin/sh
# Verifies that the node image formats filesystems that the oldest supported
# node kernel, Linux 5.15, can mount (issue #133).  The Dockerfile runs it
# while building the node image, so an xfsprogs or e2fsprogs upgrade that
# turns on a newer on-disk feature by default fails the build.
#
# XFS: the node plugin passes the options of the xfsprogs Linux 5.15 LTS
# profile (internal/csi xfsCompatProfile); the filesystem made with that
# profile must carry only the superblock feature bits Linux 5.15 knows
# (XFS_SB_FEAT_*_ALL in fs/xfs/libxfs/xfs_format.h of v5.15).
# ext4: the node plugin runs mkfs.ext4 -F -m0 with the mke2fs.conf defaults;
# every feature must be one Linux 5.15 mounts read-write.
set -eu

readonly xfs_profile=/usr/share/xfsprogs/mkfs/lts_5.15.conf
readonly image=/tmp/mkfs-baseline.img
trap 'rm -f "${image}"' EXIT

truncate -s 512M "${image}"
mkfs.xfs -q -f -c options="${xfs_profile}" "${image}"
eval "$(xfs_db -r -c 'sb 0' \
  -c 'p features_compat features_ro_compat features_incompat features_log_incompat' \
  "${image}" | sed 's/ = /=/')"
# ro_compat: FINOBT RMAPBT REFLINK INOBTCNT; incompat: FTYPE SPINODES
# META_UUID BIGTIME NEEDSREPAIR.
if [ $((features_compat)) -ne 0 ] ||
  [ $((features_ro_compat & ~0xf)) -ne 0 ] ||
  [ $((features_incompat & ~0x1f)) -ne 0 ] ||
  [ $((features_log_incompat)) -ne 0 ]; then
  echo "mkfs.xfs with ${xfs_profile} sets features Linux 5.15 cannot mount:" \
    "compat=${features_compat} ro_compat=${features_ro_compat}" \
    "incompat=${features_incompat} log_incompat=${features_log_incompat}" >&2
  exit 1
fi

mkfs.ext4 -q -F -m0 "${image}"
features=$(dumpe2fs -h "${image}" 2>/dev/null | sed -n 's/^Filesystem features: *//p')
for feature in ${features}; do
  # Features Linux 5.15 mounts read-write (fs/ext4/ext4.h *_SUPP of v5.15).
  case "${feature}" in
  64bit | bigalloc | casefold | dir_index | dir_nlink | dir_prealloc | ea_inode | \
    encrypt | ext_attr | extent | extra_isize | fast_commit | filetype | flex_bg | \
    has_journal | huge_file | imagic_inodes | inline_data | large_dir | large_file | \
    meta_bg | metadata_csum | metadata_csum_seed | mmp | orphan_file | project | \
    quota | resize_inode | sparse_super | sparse_super2 | stable_inodes | \
    uninit_bg | verity) ;;
  *)
    echo "mkfs.ext4 enables feature ${feature}, which is not in the Linux 5.15 baseline" >&2
    exit 1
    ;;
  esac
done
echo "mkfs baseline Linux 5.15: xfs ok; ext4 ok (${features})"
