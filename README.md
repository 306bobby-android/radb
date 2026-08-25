# radb

Lend a USB-attached Android device to a machine that cannot reach it, so stock
`adb` and `fastboot` running there drive it as if it were plugged in.

Built for the case where the device is on a desktop and the work — an agent, a
build, a test run — happens on a remote server.

## Why there are two halves

**adb needs no protocol work.** `adb` is already split into a client and a
server that talk over TCP, and the client will happily talk to a server on
another host. Point `ADB_SERVER_SOCKET` at a tunnelled port 5037 and
`shell`, `push`, `pull`, `install` and `logcat` all work — file transfer
included, since it rides the same connection.

**fastboot has no such split.** It drives USB directly. But it *does* know how
to reach a bootloader over TCP (`fastboot -s tcp:HOST:PORT`), and that protocol
is simple: a four byte `FB01` handshake each way, then packets prefixed with an
unsigned eight byte big-endian length. So `radb serve` pretends to be a
network-attached bootloader and relays each packet to the real one over USB.
The `fastboot` binary on the remote machine is stock and unmodified.

## Install

Needs Go and libusb.

    go build -o ~/bin/radb ./cmd/radb

## Use

On the machine with the device plugged in:

    radb serve                 # fastboot bridge on 127.0.0.1:5554, plus adb's server
    radb link me@build-box     # ssh reverse tunnel for 5037 and 5554, kept alive

On the remote machine:

    export ADB_SERVER_SOCKET=tcp:127.0.0.1:5037
    export RADB_FASTBOOT=tcp:127.0.0.1:5554

    adb shell getprop ro.product.model
    fastboot -s "$RADB_FASTBOOT" getvar product

Copy `shim/fastboot` somewhere early on the remote `PATH` and the `-s` flag
stops being your problem — plain `fastboot ...` reaches the bridged device,
which matters when the thing typing the commands is an agent that has read the
normal fastboot documentation.

`radb doctor` checks each moving part and prints the remote-side setup.

## Things that will bite you

**The adb client and server versions must match.** A client that finds a server
of a different version kills it and starts its own — which it cannot do across a
tunnel, so a version skew shows up as a connection that dies on every command.
`radb doctor` prints the local version to compare against.

**`fastboot reboot bootloader` and `reboot fastboot` report failure even when
they work.** After the reboot the client waits for the device to come back, and
over TCP it has no way to watch USB re-enumerate. The reboot itself succeeds;
just re-run the next command once the device is up. This is a limitation of
fastboot's TCP transport, not of the bridge.

**One client at a time.** A USB interface can only be claimed once. A second
`fastboot` waits for the first to disconnect.

**USB permissions.** Access to the device node usually comes from the ACL
`systemd-logind` grants the active login session, which is why this works
without root from a desktop session. A `radb serve` started as a system service
with no session has no such ACL and needs a udev rule granting your group access
to the bootloader (`18d1:4ee0` for Pixels).

**Which fastboot commands exist is up to the device.** `fetch` needs userspace
fastbootd and an unlocked device, and bootloaders restrict which partitions it
will read. `get_staged` is frequently unimplemented. The bridge relays whatever
the bootloader says, including its refusals.

## Verified against

A Pixel 5 (`redfin`), unlocked, over the bridge with stock `fastboot` 37.0.0:

- `getvar` including `getvar all` — 186 INFO lines relayed in order
- download data phase — `stage` of 32 MiB at ~18 MB/s
- upload data phase — `fetch vendor_boot_b`, 96 MiB, twice, byte-identical
  (`sha256 eb3058d8…`), ~40 MB/s
- `FAIL` relaying, `reboot`, `reboot fastboot`

The unit tests cover the packet framing and the command state machine against a
scripted bootloader, including the invariant that makes `fetch` work: an upload
payload has to leave as exactly one packet, because the host reads it in chunks
of up to 1 MiB, its TCP transport never reads across a packet boundary, and its
`ReadBuffer` treats a short read as fatal.

## Prior art

`adb` alone is covered by [adb-proxy](https://github.com/paulo-raca/adb-proxy)
and [remote-adb](https://github.com/nisargjhaveri/remote-adb).
[remote-fastboot](https://github.com/geo-stark/remote-fastboot) is the same idea
as the bridge here, in C, unmaintained.
