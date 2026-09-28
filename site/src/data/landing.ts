// Landing page copy for pillar-csi.bhyoo.com.
// Every claim here is checked against v0.3.0. `evidence` is one repo-relative
// path, optionally followed by ': ' and a short note.

export const landing = {
  meta: {
    title: 'pillar-csi: ZFS and LVM over NVMe-oF for Kubernetes',
    description:
      'One Kubernetes CSI driver for ZFS and LVM pools over kernel NVMe-oF/TCP. Its images carry the tools, so homelab and bare-metal hosts need only kernel modules.',
  },
  hero: {
    eyebrow: 'KUBERNETES CSI DRIVER · NVMe-oF/TCP',
    h1: 'Export your ZFS and LVM pools to Kubernetes.',
    h1Alternatives: [
      'Block storage for bare-metal Kubernetes, kernel to pod.',
      'Turn your storage box into Kubernetes volumes.',
      'Serve zvols and LVs to any Kubernetes node.',
      'Your NAS pool, attached over kernel NVMe-oF/TCP.',
    ],
    subline:
      'One CSI driver serves ZFS zvols and LVM volumes to your Kubernetes nodes over kernel NVMe-oF/TCP. Its images bring their own tools, so homelab and bare-metal hosts need only kernel modules and a pool.',
    installCommand:
      "helm install pillar-csi oci://ghcr.io/isac322/charts/pillar-csi --version 0.3.0 --namespace pillar-csi --create-namespace --set 'agent.backends[0].zfs.pool=tank'",
  },
  star: {
    banner: 'One person builds pillar-csi. A GitHub star helps other self-hosters find it.',
    button: 'Star on GitHub',
    reason:
      'A star helps pillar-csi show up when people search GitHub for Kubernetes storage, and it tells the maintainer that someone runs it.',
    repo: 'isac322/pillar-csi',
  },
  negations: [
    {
      value: '0',
      label: 'SSH keys to your storage host',
      evidence: 'go.mod: no SSH client library; the controller talks gRPC to the agent',
    },
    {
      value: '0',
      label: 'userspace hops in the data path',
      evidence: 'internal/csi/nvmeof_connector.go: kernel initiator connects to the kernel nvmet target',
    },
    {
      value: '1',
      label: 'CSI driver and Helm release for all your pools',
      evidence: 'charts/pillar-csi/values.yaml: every pool is an entry in agent.backends',
    },
    {
      value: 'Every',
      label: 'configfs value is read back and checked',
      evidence: 'internal/agent/nvmeof/configfs.go: writeFile compares the read-back value',
    },
  ],
  matrix: {
    heading: 'One driver, one configuration shape',
    body: 'PillarStore and PillarProtocol set defaults, PillarStorageClass overrides them, and a PVC annotation overrides that, with the same keys at each level. iSCSI, NFS and SMB are planned.',
    columns: [
      { name: 'NVMe-oF/TCP', status: 'shipped' },
      { name: 'iSCSI', status: 'planned' },
      { name: 'NFS', status: 'planned' },
      { name: 'SMB', status: 'planned' },
    ],
    rows: [
      { name: 'ZFS zvol', status: 'shipped', cells: ['shipped', 'planned', 'na', 'na'] },
      { name: 'LVM LV', status: 'shipped', cells: ['shipped', 'planned', 'na', 'na'] },
      { name: 'ZFS dataset', status: 'planned', cells: ['na', 'na', 'planned', 'planned'] },
    ],
    legend: {
      shipped: 'Shipped in v0.3.0',
      planned: 'Planned',
      na: 'Does not apply',
    },
  },
  steps: [
    {
      title: 'Install the chart',
      body: 'Hosts need a ZFS pool or LVM volume group and the kernel modules listed in the prerequisites. The images carry every userspace tool. List your pools in agent.backends.',
      moreHref: '/docs/reference/prerequisites/',
      moreLabel: 'Kernel module prerequisites',
      code: `cat > values.yaml <<'EOF'
agent:
  backends:
    - zfs: {pool: tank}
    # - lvm: {volumeGroup: data-vg}
EOF

helm install pillar-csi oci://ghcr.io/isac322/charts/pillar-csi \\
  --version 0.3.0 \\
  --namespace pillar-csi --create-namespace \\
  -f values.yaml`,
      lang: 'sh',
    },
    {
      title: 'Declare the pool',
      body: 'A PillarStore names the pool, and a PillarStorageClass turns it into a Kubernetes StorageClass. The quickstart adds the PillarAgent and PillarProtocol they reference.',
      moreHref: '/docs/tutorials/first-pvc/',
      moreLabel: 'Complete example in the quickstart',
      code: `apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStore
metadata: {name: nas-tank}
spec:
  agentRef: nas                  # PillarAgent for the storage node
  backend: {zfs: {pool: tank}}   # must match agent.backends
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata: {name: tank}           # also the StorageClass name
spec: {storeRef: nas-tank, protocolRef: nvmeof-tcp}`,
      lang: 'yaml',
    },
    {
      title: 'Claim a volume',
      body: "pillar-csi creates a zvol in tank and exports it. The pod's node connects, formats it as ext4, and mounts it.",
      code: `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: tank
  resources:
    requests:
      storage: 10Gi`,
      lang: 'yaml',
    },
  ],
  comparison: {
    checkedOn: '2026-09-28',
    columns: ['pillar-csi', 'democratic-csi', 'Longhorn', 'OpenEBS LocalPV'],
    rows: [
      {
        label: 'Data path',
        cells: [
          'Kernel nvmet target to the kernel NVMe-oF/TCP initiator',
          'Depends on the driver: NFS, iSCSI, SMB or NVMe-oF target',
          'Longhorn engine process per volume, writing to replicas',
          'Local zvol or LV on the node running the pod',
        ],
      },
      {
        label: 'How the target is configured',
        cells: [
          'pillar-agent writes configfs and reads it back',
          'Commands over SSH (targetcli, nvmetcli) or the TrueNAS API',
          'Longhorn manager',
          'No network target',
        ],
      },
      {
        label: 'Userspace tools on the hosts',
        cells: [
          'None; kernel modules only',
          'nvmetcli and SSH access on the storage host; initiator tools on workers',
          'open-iscsi on every node',
          'ZFS or LVM userspace tools on each node',
        ],
      },
      {
        label: 'Protocols',
        cells: ['NVMe-oF/TCP; iSCSI, NFS and SMB planned', 'NFS, iSCSI, SMB, NVMe-oF', 'iSCSI frontend on the consuming node', 'None, local only'],
      },
    ],
  },
  limits:
    'pillar-csi does not replicate data. Each volume lives on one storage node; when that node is down its volumes stay offline.',
  cta: {
    primary: { label: 'Read the quickstart', href: '/docs/tutorials/first-pvc/' },
    secondary: { label: 'View on GitHub', href: 'https://github.com/isac322/pillar-csi' },
  },
};
