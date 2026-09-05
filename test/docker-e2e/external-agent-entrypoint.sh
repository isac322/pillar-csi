#!/bin/sh
set -eu

vg_name=${PILLAR_E2E_LVM_VG:-pillar-e2e-vg}
backing_file=/var/lib/pillar-csi/${vg_name}.img
mkdir -p /var/lib/pillar-csi
truncate -s 2G "${backing_file}"
loop_device=$(losetup --find --show "${backing_file}")
pvcreate --force --yes "${loop_device}"
vgcreate "${vg_name}" "${loop_device}"

exec /usr/local/bin/pillar-agent \
  --listen-address=0.0.0.0:9500 \
  --backend="type=lvm-lv,vg=${vg_name}"
