#!/bin/bash
# apehost-firerunners host setup for podbox (Ubuntu 24.04, Docker already present).
# Additive only: own containerd (1.7, matches the fireactions client) on its own
# socket + RAM-backed devmapper thin-pool, so Docker's containerd (2.x,
# /run/containerd) is never touched. Idempotent. Run as root.
set -euo pipefail

FC=1.17.0 CTR=1.7.36 CNI=1.9.1 KERNEL=6.18.51
KERNEL_URL=https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/20261007-f23a2136011e-0/x86_64/vmlinux-$KERNEL
SUBNET=10.200.0.0/24   # 192.168.128.0/24 (upstream default) collides with a docker bridge here
POOL=fireactions-thinpool
CTR_ROOT=/var/lib/fireactions-containerd
DM_DIR=$CTR_ROOT/devmapper
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT

# --- firecracker
if ! firecracker --version 2>/dev/null | grep -q "v$FC"; then
  curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/v$FC/firecracker-v$FC-x86_64.tgz" | tar -xz -C "$T"
  install -m755 "$T/release-v$FC-x86_64/firecracker-v$FC-x86_64" /usr/local/bin/firecracker
fi

# --- dedicated containerd 1.7
if ! /opt/fireactions/bin/containerd --version 2>/dev/null | grep -q "v$CTR"; then
  mkdir -p /opt/fireactions/bin
  curl -fsSL "https://github.com/containerd/containerd/releases/download/v$CTR/containerd-$CTR-linux-amd64.tar.gz" | tar -xz -C "$T"
  install -m755 "$T/bin/containerd" "$T/bin/ctr" /opt/fireactions/bin/
fi

# --- CNI (fireactions hardcodes /opt/cni/bin, /etc/cni/net.d, network "fireactions")
if [ ! -x /opt/cni/bin/bridge ] || ! /opt/cni/bin/bridge 2>&1 | grep -q "v$CNI"; then
  mkdir -p /opt/cni/bin
  curl -fsSL "https://github.com/containernetworking/plugins/releases/download/v$CNI/cni-plugins-linux-amd64-v$CNI.tgz" | tar -xz -C /opt/cni/bin
fi
[ -x /opt/cni/bin/tc-redirect-tap ] || {
  curl -fsSL -o /opt/cni/bin/tc-redirect-tap https://github.com/hostinger/tc-redirect-tap/releases/download/v0.0.1/tc-redirect-tap-amd64
  chmod 755 /opt/cni/bin/tc-redirect-tap
}
mkdir -p /etc/cni/net.d
cat > /etc/cni/net.d/10-fireactions.conflist <<EOF
{
  "cniVersion": "0.4.0",
  "name": "fireactions",
  "plugins": [
    {
      "type": "bridge", "bridge": "fireactions-br0", "isDefaultGateway": true,
      "ipMasq": true, "hairpinMode": true, "mtu": 1500,
      "ipam": { "type": "host-local", "subnet": "$SUBNET", "dataDir": "/var/run/cni",
                "resolvConf": "/run/systemd/resolve/resolv.conf" }
    },
    { "type": "firewall" },
    { "type": "tc-redirect-tap" }
  ]
}
EOF

# --- guest kernel (Firecracker CI build; 5.10/6.1 are past end of support)
mkdir -p /var/lib/fireactions/kernels/6.18
[ -s /var/lib/fireactions/kernels/6.18/vmlinux ] || curl -fsSL -o /var/lib/fireactions/kernels/6.18/vmlinux "$KERNEL_URL"

# --- thin-pool on sparse loop files in RAM (tmpfs). Runners are ephemeral, so the
# whole containerd root (images, snapshots, metadata) starts clean each boot and
# fireactions re-pulls the image. tmpfs only consumes RAM for written pages, and
# discard_blocks hands freed blocks back after each VM.
cat > /usr/local/sbin/fireactions-thinpool <<EOF
#!/bin/bash
# RAM-backed storage cap; budget guest RAM separately when adding pool replicas.
set -euo pipefail
dmsetup info $POOL >/dev/null 2>&1 && exit 0
mkdir -p $CTR_ROOT
mountpoint -q $CTR_ROOT || mount -t tmpfs -o size=36G,mode=0700 tmpfs-fireactions $CTR_ROOT
mkdir -p $DM_DIR
[ -f $DM_DIR/data ] || truncate -s 32G $DM_DIR/data
[ -f $DM_DIR/meta ] || truncate -s 2G $DM_DIR/meta
DATA=\$(losetup --find --show $DM_DIR/data)
META=\$(losetup --find --show $DM_DIR/meta)
dmsetup create $POOL --table "0 \$((\$(blockdev --getsize64 \$DATA) / 512)) thin-pool \$META \$DATA 128 32768"
EOF
chmod 755 /usr/local/sbin/fireactions-thinpool

mkdir -p /etc/fireactions
cat > /etc/fireactions/containerd.toml <<EOF
version = 2
root = "$CTR_ROOT"
state = "/run/fireactions-containerd"
disabled_plugins = ["io.containerd.grpc.v1.cri"]
[grpc]
  address = "/run/fireactions-containerd/containerd.sock"
[ttrpc]
  address = "/run/fireactions-containerd/containerd.sock.ttrpc"
[plugins."io.containerd.snapshotter.v1.devmapper"]
  pool_name = "$POOL"
  root_path = "$DM_DIR"
  base_image_size = "20GB"
  discard_blocks = true
EOF

cat > /etc/systemd/system/fireactions-thinpool.service <<'EOF'
[Unit]
Description=Fireactions devmapper thin-pool (loop-backed)
Before=fireactions-containerd.service
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/fireactions-thinpool
[Install]
WantedBy=multi-user.target
EOF

cat > /etc/systemd/system/fireactions-containerd.service <<'EOF'
[Unit]
Description=containerd for Fireactions (separate from Docker's)
Requires=fireactions-thinpool.service
After=network.target fireactions-thinpool.service
[Service]
ExecStart=/opt/fireactions/bin/containerd --config /etc/fireactions/containerd.toml
Delegate=yes
KillMode=process
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
EOF

cat > /etc/systemd/system/fireactions.service <<'EOF'
[Unit]
Description=Fireactions (Firecracker GitHub runners)
Documentation=https://github.com/JungleM0nkey/apehost-firerunners
Requires=fireactions-containerd.service
After=network-online.target fireactions-containerd.service
[Service]
ExecStart=/usr/local/bin/fireactions server --config /etc/fireactions/config.yaml
KillMode=process
Restart=always
RestartSec=10
[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now fireactions-thinpool.service fireactions-containerd.service
echo "firecracker: $(firecracker --version | head -1)"
echo "containerd:  $(/opt/fireactions/bin/containerd --version)"
/opt/fireactions/bin/ctr -a /run/fireactions-containerd/containerd.sock plugins ls | grep -E "devmapper"
dmsetup status $POOL
