package main

import (
	"flag"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/bobbypanarisi/radb/internal/fastboot"
	"github.com/bobbypanarisi/radb/internal/remote"
)

// doctor reports on each moving part of the setup, in the order a failure would
// bite: the tools, the adb server, the device, then the bridge.
func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	adbPort := fs.Int("adb-port", remote.DefaultADBPort, "adb server port to probe")
	fbPort := fs.Int("fastboot-port", remote.DefaultFastbootPort, "fastboot bridge port to probe")
	fs.Parse(args)

	adbVer := checkADB()
	checkADBServer(*adbPort)
	checkADBDevices()
	checkFastbootUSB()
	checkBridge(*fbPort)

	fmt.Println()
	fmt.Println("On the remote machine:")
	fmt.Printf("  export ADB_SERVER_SOCKET=tcp:127.0.0.1:%d\n", *adbPort)
	fmt.Printf("  export RADB_FASTBOOT=tcp:127.0.0.1:%d\n", *fbPort)
	if adbVer != "" {
		fmt.Println()
		fmt.Printf("  The remote adb client must be %s as well. A client that finds a\n", adbVer)
		fmt.Println("  different server version kills the server and starts its own -- which it")
		fmt.Println("  cannot do across the tunnel, so the mismatch shows up as a dead connection.")
	}
	return nil
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
