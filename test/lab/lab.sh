#!/bin/bash
# End-to-end test lab for go-zfs-rebalance: an Ubuntu VM with OpenZFS and a TrueNAS SCALE VM, run
# by an ordinary user with QEMU user-mode networking. Everything lives in $LAB_DIR; nothing on the
# host is reconfigured. See test/lab/README.md for the full walkthrough and test plan.
#
# Forwarded ports (127.0.0.1 only): 2221 Linux VM ssh, 2222 TrueNAS ssh, 18080 TrueNAS http,
# 18000 release-file server (the VMs reach it as http://10.0.2.2:18000).
set -euo pipefail

LAB_DIR=${LAB_DIR:-$HOME/rebalance-lab}
TRUENAS_VERSION=${TRUENAS_VERSION:-25.10.7}
TRUENAS_TRAIN=${TRUENAS_TRAIN:-TrueNAS-SCALE-Goldeye}
UBUNTU_SERIES=${UBUNTU_SERIES:-noble}
DISK_SIZE=${DISK_SIZE:-10G}
DATA_DISKS_LINUX=6      # DATA1-4: main pool, DATA5-6: a second pool for side-by-side tests
DATA_DISKS_TRUENAS=4
HERE=$(cd "$(dirname "$0")" && pwd)

UBUNTU_IMG="$LAB_DIR/images/$UBUNTU_SERIES-server-cloudimg-amd64.img"
TRUENAS_ISO="$LAB_DIR/images/TrueNAS-SCALE-$TRUENAS_VERSION.iso"
SSH_OPTS="-i $LAB_DIR/lab_key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"

mkdir -p "$LAB_DIR"
cd "$LAB_DIR"

running() { [ -f "$1/qemu.pid" ] && kill -0 "$(cat "$1/qemu.pid")" 2>/dev/null; }

setup() {
    for t in qemu-system-x86_64 qemu-img xorriso python3 curl sha256sum; do
        command -v "$t" >/dev/null || { echo "missing: $t"; exit 1; }
    done
    [ -r /dev/kvm ] || { echo "no access to /dev/kvm (add yourself to the kvm group)"; exit 1; }
    [ -f /usr/share/OVMF/OVMF_CODE_4M.fd ] || { echo "missing UEFI firmware (package ovmf)"; exit 1; }
    mkdir -p images linux truenas logs dist pylib
    [ -f lab_key ] || ssh-keygen -q -t ed25519 -N "" -C rebalance-lab -f lab_key
    cat lab_key.pub > authorized_keys
    for k in "$HOME"/.ssh/*.pub; do [ -f "$k" ] && cat "$k" >> authorized_keys; done
    cp "$HERE"/tn.py "$HERE"/manifest.py "$HERE"/datagen.py .
    [ -f truenas/password ] || (umask 077; head -c 18 /dev/urandom | base64 | tr -d '/+=' > truenas/password)

    echo "Downloading the Ubuntu $UBUNTU_SERIES cloud image (~600 MB)..."
    [ -f "$UBUNTU_IMG" ] || curl -fL -o "$UBUNTU_IMG" \
        "https://cloud-images.ubuntu.com/$UBUNTU_SERIES/current/$UBUNTU_SERIES-server-cloudimg-amd64.img"
    echo "Downloading TrueNAS SCALE $TRUENAS_VERSION (~2.3 GB) and checking it..."
    local base="https://download.truenas.com/$TRUENAS_TRAIN/$TRUENAS_VERSION/TrueNAS-SCALE-$TRUENAS_VERSION.iso"
    [ -f "$TRUENAS_ISO" ] || curl -fL -o "$TRUENAS_ISO" "$base?download=1"
    curl -fsSL -o "$TRUENAS_ISO.sha256" "$base.sha256?download=1"
    [ "$(awk '{print $1}' "$TRUENAS_ISO.sha256")" = "$(sha256sum "$TRUENAS_ISO" | awk '{print $1}')" ] ||
        { echo "TrueNAS ISO checksum mismatch"; exit 1; }
    # The TrueNAS installer is driven over a websocket; use the pure-Python wheel so nothing is installed system-wide.
    if [ ! -d pylib/websockets ]; then
        url=$(curl -fsSL https://pypi.org/pypi/websockets/json | python3 -c \
            'import json,sys; print(next(u["url"] for u in json.load(sys.stdin)["urls"] if u["filename"].endswith("py3-none-any.whl")))')
        curl -fsSL -o /tmp/websockets.whl "$url"
        python3 -c 'import zipfile,sys; zipfile.ZipFile(sys.argv[1]).extractall("pylib")' /tmp/websockets.whl
        rm -f /tmp/websockets.whl
    fi
    echo "Lab ready in $LAB_DIR"
}

data_disk_args() { # dir count
    local dir=$1 n=$2 i args=""
    for i in $(seq 1 "$n"); do
        [ -f "$dir/d$i.qcow2" ] || qemu-img create -q -f qcow2 "$dir/d$i.qcow2" "$DISK_SIZE"
        args="$args -drive if=none,id=d$i,file=$dir/d$i.qcow2,format=qcow2,discard=unmap"
        args="$args -device virtio-blk-pci,drive=d$i,serial=DATA$i"
    done
    echo "$args"
}

linux_seed() {
    local keys
    keys=$(sed 's/^/      - /' authorized_keys)
    cat > linux/user-data <<EOF
#cloud-config
hostname: rb-linux
users:
  - name: lab
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
$keys
  - name: alice
    shell: /bin/bash
  - name: bob
    shell: /bin/bash
package_update: true
packages: [zfsutils-linux, attr, acl, tmux, python3, jq]
runcmd:
  - [touch, /var/lib/cloud/lab-ready]
EOF
    printf 'instance-id: rb-linux-1\nlocal-hostname: rb-linux\n' > linux/meta-data
    xorriso -as mkisofs -quiet -output linux/seed.iso -volid cidata -joliet -rock linux/user-data linux/meta-data
}

up_linux() {
    running linux && { echo "Linux VM already running"; return; }
    [ -f linux/os.qcow2 ] || qemu-img create -q -f qcow2 -F qcow2 -b "$UBUNTU_IMG" linux/os.qcow2 30G
    [ -f linux/seed.iso ] || linux_seed
    # shellcheck disable=SC2046
    qemu-system-x86_64 -name rb-linux -machine q35,accel=kvm -cpu host -smp 4 -m 4096 \
        -drive if=none,id=os,file=linux/os.qcow2,format=qcow2 -device virtio-blk-pci,drive=os,bootindex=0 \
        -drive if=none,id=seed,file=linux/seed.iso,format=raw,readonly=on -device virtio-blk-pci,drive=seed \
        $(data_disk_args linux "$DATA_DISKS_LINUX") \
        -netdev user,id=n0,hostfwd=tcp:127.0.0.1:2221-:22 -device virtio-net-pci,netdev=n0 \
        -display none -serial file:linux/serial.log -daemonize -pidfile linux/qemu.pid
    echo "Linux VM starting; waiting for cloud-init to install OpenZFS..."
    for _ in $(seq 1 90); do
        # shellcheck disable=SC2086
        ssh $SSH_OPTS -p 2221 lab@127.0.0.1 test -f /var/lib/cloud/lab-ready 2>/dev/null && {
            # shellcheck disable=SC2086
            scp $SSH_OPTS -P 2221 manifest.py datagen.py lab@127.0.0.1:/tmp/
            # shellcheck disable=SC2086
            ssh $SSH_OPTS -p 2221 lab@127.0.0.1 sudo install -m 755 /tmp/manifest.py /tmp/datagen.py /usr/local/sbin/
            echo "Linux VM ready"; return; }
        sleep 10
    done
    echo "Linux VM didn't become ready; see $LAB_DIR/linux/serial.log"; exit 1
}

up_truenas() { # install|run
    running truenas && { echo "TrueNAS VM already running"; return; }
    [ -f truenas/boot.qcow2 ] || qemu-img create -q -f qcow2 truenas/boot.qcow2 32G
    [ -f truenas/OVMF_VARS.fd ] || cp /usr/share/OVMF/OVMF_VARS_4M.fd truenas/OVMF_VARS.fd
    local cd=""
    [ "$1" = install ] && cd="-drive file=$TRUENAS_ISO,media=cdrom,readonly=on,if=none,id=cd0 -device ide-cd,drive=cd0,bootindex=0"
    # shellcheck disable=SC2046,SC2086
    qemu-system-x86_64 -name rb-truenas -machine q35,accel=kvm -cpu host -smp 4 -m 8192 \
        -drive if=pflash,format=raw,readonly=on,file=/usr/share/OVMF/OVMF_CODE_4M.fd \
        -drive if=pflash,format=raw,file=truenas/OVMF_VARS.fd \
        -drive if=none,id=boot,file=truenas/boot.qcow2,format=qcow2 -device virtio-blk-pci,drive=boot,serial=BOOT1,bootindex=1 \
        $cd $(data_disk_args truenas "$DATA_DISKS_TRUENAS") \
        -netdev user,id=n0,hostfwd=tcp:127.0.0.1:2222-:22,hostfwd=tcp:127.0.0.1:18080-:80 -device virtio-net-pci,netdev=n0 \
        -display none -serial file:truenas/serial.log -daemonize -pidfile truenas/qemu.pid
    echo "TrueNAS VM started ($1)"
}

truenas_install() {
    # Hands-off install through the TrueNAS installer's JSON-RPC API, then first boot and SSH setup.
    up_truenas install
    ./tn.py install
    down truenas
    up_truenas run
    ./tn.py setup
    for _ in $(seq 1 60); do
        # shellcheck disable=SC2086
        ssh $SSH_OPTS -p 2222 truenas_admin@127.0.0.1 'sudo -n true' 2>/dev/null && {
            # shellcheck disable=SC2086
            scp $SSH_OPTS -P 2222 manifest.py datagen.py truenas_admin@127.0.0.1:/tmp/
            # shellcheck disable=SC2086
            ssh $SSH_OPTS -p 2222 truenas_admin@127.0.0.1 'sudo mkdir -p /root/lab && sudo install -m 755 /tmp/manifest.py /tmp/datagen.py /root/lab/'
            echo "TrueNAS VM ready"; return; }
        sleep 10
    done
    echo "TrueNAS SSH never came up; see $LAB_DIR/truenas/serial.log"; exit 1
}

serve_dist() { # dir with make-dist output
    cp "$1"/* dist/
    (cd dist && sha256sum -c checksums.txt)
    pkill -f "http.server 18000" 2>/dev/null || true
    setsid nohup python3 -m http.server 18000 --bind 127.0.0.1 --directory dist > logs/dist-http.log 2>&1 < /dev/null &
    echo "Serving $(find dist -type f | wc -l | tr -d " ") release files at http://10.0.2.2:18000/ (from inside the VMs)"
}

down() { # dir
    running "$1" || { echo "$1 not running"; return; }
    kill "$(cat "$1/qemu.pid")"; sleep 3; echo "$1 stopped"
}

# shellcheck disable=SC2086  # SSH_OPTS is deliberately word-split
case "${1:-}" in
    setup) setup ;;
    up-linux) up_linux ;;
    truenas-install) truenas_install ;;
    up-truenas) up_truenas run ;;
    serve-dist) shift; serve_dist "${1:?usage: lab.sh serve-dist DIR}" ;;
    down-linux) down linux ;;
    down-truenas) down truenas ;;
    ssh-linux) shift; exec ssh $SSH_OPTS -p 2221 lab@127.0.0.1 "$@" ;;
    ssh-truenas) shift; exec ssh $SSH_OPTS -p 2222 truenas_admin@127.0.0.1 "$@" ;;
    status) for d in linux truenas; do if running $d; then echo "$d: running"; else echo "$d: stopped"; fi; done ;;
    destroy) down linux || true; down truenas || true; rm -rf linux truenas; echo "VM disks removed (downloads kept)" ;;
    *) sed -n '2,7p' "$0"; echo
       echo "usage: $0 setup | up-linux | truenas-install | up-truenas | serve-dist DIR | ssh-linux [cmd] |"
       echo "          ssh-truenas [cmd] | down-linux | down-truenas | status | destroy"
       exit 2 ;;
esac
