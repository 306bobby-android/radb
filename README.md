# radb

Lend a USB-attached Android device to a machine that cannot reach it, so stock
`adb` and `fastboot` running there drive it as if it were plugged in.

Built for the case where the device is on a desktop and the work -- an agent, a
build, a test run -- happens on a remote server.

## Why there are two halves

**adb needs no protocol work.** `adb` is already split into a client and a
server that talk over TCP, and the client will happily talk to a server on
another host. Point `ADB_SERVER_SOCKET` at a tunnelled port 5037 and `shell`,
`push`, `pull`, `install` and `logcat` all work -- file transfer included, since
it rides the same connection.

**fastboot has no such split.** It drives USB directly. But it *does* know how
to reach a bootloader over TCP (`fastboot -s tcp:HOST:PORT`), and that protocol
is simple: a four byte `FB01` handshake each way, then packets prefixed with an
unsigned eight byte big-endian length. So `radb serve` pretends to be a
network-attached bootloader and relays each packet to the real one over USB.
The `fastboot` binary on the remote machine is stock and unmodified.

## What goes where

**radb only ever runs on the machine with the device.** The remote server needs
no part of it -- no Go, no libusb, no root, no daemon. It runs the stock tools
against ports that ssh puts on its loopback.

On the device host:

- `radb`, built with Go and libusb: `go build -o ~/.local/bin/radb ./cmd/radb`
- `adb`, whose server radb proxies
- the device, and a login session (see the note on USB permissions below)

On the remote server:

- `adb` and `fastboot` from platform-tools
- `adb version` must report the same number as it does on the device host
- optionally `shim/rfastboot`, a single shell script, copied anywhere on `PATH`

## Use

One command on the device host:

    radb link me@build-box

That starts the adb server if it is not up, the adb proxy on 5038, the fastboot
bridge on 5554, and an ssh reverse tunnel that reconnects on its own. The real
adb server keeps 5037 to itself, so anything local carries on unaffected; the
tunnel points the remote's 5037 at the proxy and its 5554 at the bridge.

`radb serve` runs just the local half, for when something else -- a VPN, a
systemd unit, your own ssh -- carries the ports. `radb link -serve=false` is the
other side of that, when the local half is already running.

Then on the remote server:

    export ADB_SERVER_SOCKET=tcp:127.0.0.1:5037
    export RADB_FASTBOOT=tcp:127.0.0.1:5554

    adb shell getprop ro.product.model
    adb push ./build/app.apk /data/local/tmp/
    fastboot -s "$RADB_FASTBOOT" getvar product

Use `127.0.0.1` rather than `localhost`: ssh binds the forward on IPv4 only, so
a client that resolves `localhost` to `::1` first will find nothing there.

Nothing special is needed in the server's sshd config. The forwards bind its
loopback, which stock `AllowTcpForwarding yes` and `GatewayPorts no` already
permit.

`radb doctor` checks each moving part and prints the remote-side setup.

### rfastboot

`adb` picks the device up from the environment, but `fastboot` has no equivalent
variable -- it only takes a device on the command line, so `-s` has to be on
every invocation, and forgetting it produces `no devices/emulators found`, which
reads like an unplugged phone rather than a missing flag.

`shim/rfastboot` supplies it:

    rfastboot getvar product
    rfastboot flash boot boot.img

Copy it anywhere on the remote `PATH`. It deliberately does not shadow
`fastboot`, so the real binary still means the real binary and a server with its
own USB devices behaves normally. An explicit `-s` passes straight through, and
`RADB_REAL_FASTBOOT` pins which binary it wraps.

## Things that will bite you

**A mismatched adb client will try to kill your adb server.** This is the one
failure worth understanding, because the client does real damage before it
reports anything useful. When the version it expects differs from the server's,
it sends `host:kill` and starts a replacement -- which across a tunnel means it
takes down the server every other session is using and cannot put one back.

`radb serve` therefore puts a proxy in front of the adb server and points the
tunnel at that instead. The proxy refuses `host:kill`, so the server survives,
and answers with a message the client prints verbatim:

    adb server version (40) doesn't match this client (41); killing...
    error: radb refused to kill this adb server: it is shared over a tunnel and
    other sessions depend on it. Your adb disagrees with its version (this server
    reports 40), so install platform-tools whose adb matches and retry.

The proxy never rewrites the version itself. That number is a compatibility
contract, and lying about it would trade a loud failure for a quiet one.

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

**A device in the bootloader is invisible to adb.** A remote `adb devices` goes
quiet at exactly the moment you want to know where the device went. The proxy
fills that silence in, which is what the extra entries below are for.

**Which fastboot commands exist is up to the device.** `fetch` needs userspace
fastbootd and an unlocked device, and bootloaders restrict which partitions it
will read. `get_staged` is frequently unimplemented. The bridge relays whatever
the bootloader says, including its refusals.

## The device list as a status channel

Both columns of `adb devices` are free text that the client prints without
interpreting, so the proxy uses them to explain states that would otherwise be an
empty list:

    List of devices attached
    0A021FDD4005CG                  device
    radb-ADB-VERSION-MISMATCH       a-client-tried-to-kill-this-v41-server-at-10:16:04

and, when the device has gone into its bootloader:

    0A021FDD4005CG                  in-fastboot-mode-use-fastboot-not-adb

Real devices are always listed first and untouched. Note the reach of each
channel: the version check runs before `host:devices`, so a client that fails it
never asks for the list and only ever sees the `error:` line above. The entry is
a breadcrumb for whoever looks next -- a healthy client, or `radb doctor`, which
reports the same incidents with full timestamps.

Pass `-inject=false` to `radb serve` if anything you run parses `adb devices`
strictly enough to object.

## Verified against

A Pixel 5 (`redfin`), unlocked, over the bridge with stock `fastboot` 37.0.0:

- `getvar` including `getvar all` -- 186 INFO lines relayed in order
- download data phase -- `stage` of 32 MiB at ~18 MB/s
- upload data phase -- `fetch vendor_boot_b`, 96 MiB, twice, byte-identical
  (`sha256 eb3058d8...`), ~40 MB/s
- `FAIL` relaying, `reboot`, `reboot fastboot`

The adb half was checked against a real client driven into a genuine version
disagreement (a stand-in server reporting 40 to a client expecting 41): the
client sent `host:kill`, the proxy refused it, zero kills reached the server, and
the client printed the explanation above.

The whole path was then run over a real ssh reverse tunnel into a throwaway sshd
left at its stock forwarding defaults -- device list, `host-features`, the
refused kill and its message, and a fastboot command reaching the bridge.

## Prior art

`adb` alone is covered by [adb-proxy](https://github.com/paulo-raca/adb-proxy)
and [remote-adb](https://github.com/nisargjhaveri/remote-adb).
[remote-fastboot](https://github.com/geo-stark/remote-fastboot) is the same idea
as the bridge here, in C, unmaintained.
