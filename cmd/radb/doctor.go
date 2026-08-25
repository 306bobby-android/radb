package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/306bobby-android/radb/internal/fastboot"
	"github.com/306bobby-android/radb/internal/remote"
)

// doctor reports on each moving part of the setup, in the order a failure would
// bite: the tools, the adb server, the device, then the bridge.
func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	adbPort := fs.Int("adb-port", remote.DefaultADBPort, "adb server port to probe")
	fbPort := fs.Int("fastboot-port", remote.DefaultFastbootPort, "fastboot bridge port to probe")
	proxyPort := fs.Int("adb-proxy-port", remote.DefaultProxyPort, "adb proxy port to probe")
	fs.Parse(args)

	adbVer := checkADB()
	checkADBServer(*adbPort)
	checkADBDevices()
	checkFastbootUSB()
	checkBridge(*fbPort)
	checkProxy(*proxyPort)

	fmt.Println()
	fmt.Println("On the remote machine:")
	fmt.Printf("  export ADB_SERVER_SOCKET=tcp:127.0.0.1:%d\n", *adbPort)
	fmt.Printf("  export RADB_FASTBOOT=tcp:127.0.0.1:%d\n", *fbPort)
	if adbVer != "" {
		fmt.Println()
		fmt.Printf("  The remote adb client should be %s. A client that disagrees about the\n", adbVer)
		fmt.Println("  server version tries to kill the server and start its own; the proxy")
		fmt.Println("  refuses, tells that client why, and records it here.")
	}
	return nil
}

// checkProxy asks the proxy for its own report, which is where refused kill
// attempts -- the fingerprint of a version mismatch -- are recorded.
func checkProxy(port int) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		warn("adb proxy not listening on %s (the tunnel would be pointed here)", addr)
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	req := "radb:status"
	if _, err := fmt.Fprintf(conn, "%04x%s", len(req), req); err != nil {
		bad("adb proxy on %s did not accept a status request: %v", addr, err)
		return
	}
	buf := make([]byte, 8192)
	n, _ := io.ReadFull(conn, buf[:8])
	if n < 8 || string(buf[:4]) != "OKAY" {
		bad("something is listening on %s but it is not the radb proxy", addr)
		return
	}
	size, err := strconv.ParseUint(string(buf[4:8]), 16, 32)
	if err != nil {
		bad("adb proxy on %s sent a malformed reply", addr)
		return
	}
	body := make([]byte, size)
	io.ReadFull(conn, body)

	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	ok("adb proxy on %s: %s", addr, lines[0])
	for _, l := range lines[1:] {
		if strings.Contains(l, "no client has tried") {
			ok("%s", l)
		} else {
			warn("%s", l)
		}
	}
}

func ok(format string, a ...any)   { fmt.Printf("  ok    %s\n", fmt.Sprintf(format, a...)) }
func bad(format string, a ...any)  { fmt.Printf("  FAIL  %s\n", fmt.Sprintf(format, a...)) }
func warn(format string, a ...any) { fmt.Printf("  warn  %s\n", fmt.Sprintf(format, a...)) }

// checkADB reports the local adb version, which both ends have to agree on.
func checkADB() string {
	out, err := exec.Command("adb", "version").Output()
	if err != nil {
		bad("adb not runnable: %v", err)
		return ""
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	ok("%s", line)
	return line
}

func checkADBServer(port int) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		bad("adb server not listening on %s (run: adb start-server)", addr)
		return
	}
	conn.Close()
	ok("adb server listening on %s", addr)
}

func checkADBDevices() {
	out, err := exec.Command("adb", "devices", "-l").Output()
	if err != nil {
		bad("adb devices failed: %v", err)
		return
	}
	var found []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n")[1:] {
		if line = strings.TrimSpace(line); line != "" {
			found = append(found, line)
		}
	}
	if len(found) == 0 {
		warn("no devices in adb mode (fine if the device is sitting in the bootloader)")
		return
	}
	for _, d := range found {
		ok("adb device: %s", d)
	}
}

func checkFastbootUSB() {
	list, err := fastboot.List()
	if err != nil {
		bad("could not enumerate USB: %v", err)
		return
	}
	if len(list) == 0 {
		warn("no devices in fastboot mode (fine if the device is booted into Android)")
		return
	}
	for _, d := range list {
		ok("bootloader: %s on usb:%s", d.Serial, d.Path)
	}
}

// checkBridge does the real handshake rather than just opening a socket, so
// that something else squatting on the port is reported as such.
func checkBridge(port int) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		bad("fastboot bridge not listening on %s (run: radb serve)", addr)
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	ver, err := fastboot.Handshake(conn)
	if err != nil {
		bad("something is listening on %s but it is not the bridge: %v", addr, err)
		return
	}
	ok("fastboot bridge on %s speaking protocol v%d", addr, ver)
}
