package adbproxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
)

// fakeServer stands in for a real adb server, answering one request per
// connection the way the host: services do.
func fakeServer(t *testing.T, reply func(service string) (status string, body []byte)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				svc, err := readFrame(c)
				if err != nil {
					return
				}
				status, body := reply(string(svc))
				io.WriteString(c, status)
				writeFrame(c, body)
			}()
		}
	}()
	return ln.Addr().String()
}

// startProxy runs a Proxy in front of upstream and returns its address.
func startProxy(t *testing.T, p *Proxy) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	go p.Serve(ctx, ln)
	return ln.Addr().String()
}

// ask sends one service request and returns the status and body.
func ask(t *testing.T, addr, service string) (string, []byte) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := writeFrame(c, []byte(service)); err != nil {
		t.Fatal(err)
	}
	var status [4]byte
	if _, err := io.ReadFull(c, status[:]); err != nil {
		t.Fatalf("reading status for %q: %v", service, err)
	}
	body, err := readFrame(c)
	if err != nil {
		t.Fatalf("reading body for %q: %v", service, err)
	}
	return string(status[:]), body
}

func versionReply(service string) (string, []byte) {
	switch {
	case service == "host:version":
		return "OKAY", []byte("0029")
	case strings.HasPrefix(service, "host:devices"):
		return "OKAY", []byte("REALSERIAL\tdevice\n")
	case service == "host:kill":
		return "OKAY", nil
	}
	return "FAIL", []byte("unknown")
}

// A client whose version disagrees with the server kills it. Across a tunnel
// that would take the server down for everyone, so the proxy must refuse.
func TestKillIsRefusedAndExplained(t *testing.T) {
	killed := false
	up := fakeServer(t, func(s string) (string, []byte) {
		if s == "host:kill" {
			killed = true
		}
		return versionReply(s)
	})

	p := &Proxy{Upstream: up}
	addr := startProxy(t, p)

	status, body := ask(t, addr, "host:kill")
	if status != "FAIL" {
		t.Errorf("status = %q, want FAIL", status)
	}
	if killed {
		t.Error("the kill reached the upstream server")
	}
	// The client prints this verbatim as "error: ...", so it has to name both
	// the cause and the fix.
	for _, want := range []string{"radb refused", "41", "platform-tools"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("message %q is missing %q", body, want)
		}
	}
}

func TestKillIsRecordedAndSurfacedAsADevice(t *testing.T) {
	p := &Proxy{Upstream: fakeServer(t, versionReply), Inject: true}
	addr := startProxy(t, p)

	// Before anything goes wrong the list is exactly what the server said.
	if _, body := ask(t, addr, "host:devices"); string(body) != "REALSERIAL\tdevice\n" {
		t.Fatalf("clean device list = %q", body)
	}

	ask(t, addr, "host:kill")

	_, body := ask(t, addr, "host:devices")
	if !strings.HasPrefix(string(body), "REALSERIAL\tdevice\n") {
		t.Errorf("the real device was disturbed: %q", body)
	}
	if !strings.Contains(string(body), "radb-ADB-VERSION-MISMATCH") {
		t.Errorf("device list %q does not report the mismatch", body)
	}
	// Every line must still be two whitespace-separated columns or adb's own
	// parser would choke on it.
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if len(strings.Fields(line)) < 2 {
			t.Errorf("line %q is not a valid device entry", line)
		}
	}
}

// A device in the bootloader is invisible to adb, which makes a remote
// `adb devices` go quiet exactly when you want to know where the device went.
func TestBootloaderDevicesAreReported(t *testing.T) {
	p := &Proxy{
		Upstream:    fakeServer(t, versionReply),
		Inject:      true,
		Bootloaders: func() []string { return []string{"0A021FDD4005CG"} },
	}
	addr := startProxy(t, p)

	_, body := ask(t, addr, "host:devices")
	if !strings.Contains(string(body), "0A021FDD4005CG\tin-fastboot-mode-use-fastboot-not-adb") {
		t.Errorf("device list %q does not mention the bootloader", body)
	}
}

func TestInjectionCanBeTurnedOff(t *testing.T) {
	p := &Proxy{
		Upstream:    fakeServer(t, versionReply),
		Inject:      false,
		Bootloaders: func() []string { return []string{"0A021FDD4005CG"} },
	}
	addr := startProxy(t, p)
	ask(t, addr, "host:kill")

	_, body := ask(t, addr, "host:devices")
	if string(body) != "REALSERIAL\tdevice\n" {
		t.Errorf("device list = %q, want it untouched", body)
	}
}

func TestLongFormKeepsItsColumns(t *testing.T) {
	p := &Proxy{
		Upstream: fakeServer(t, func(s string) (string, []byte) {
			if s == "host:devices-l" {
				return "OKAY", []byte("REALSERIAL   device product:x model:y device:z transport_id:1\n")
			}
			return versionReply(s)
		}),
		Inject:      true,
		Bootloaders: func() []string { return []string{"BOOTSER"} },
	}
	addr := startProxy(t, p)

	_, body := ask(t, addr, "host:devices-l")
	line := ""
	for _, l := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(l, "BOOTSER") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no bootloader line in %q", body)
	}
	if !strings.Contains(line, "transport_id:") {
		t.Errorf("long form line %q lacks transport_id", line)
	}
}

// Anything we do not intercept has to reach the server untouched.
func TestUnhandledServicesArePassedThrough(t *testing.T) {
	var got string
	p := &Proxy{Upstream: fakeServer(t, func(s string) (string, []byte) {
		got = s
		return "OKAY", []byte("0029")
	})}
	addr := startProxy(t, p)

	status, body := ask(t, addr, "host:version")
	if status != "OKAY" || string(body) != "0029" {
		t.Errorf("version = %q %q", status, body)
	}
	if got != "host:version" {
		t.Errorf("server saw %q", got)
	}
}

// The proxy must never invent a version: lying about it would break the
// compatibility contract the number exists to enforce.
func TestVersionIsNotRewritten(t *testing.T) {
	p := &Proxy{Upstream: fakeServer(t, func(s string) (string, []byte) {
		if s == "host:version" {
			return "OKAY", []byte("0028")
		}
		return versionReply(s)
	})}
	addr := startProxy(t, p)
	_, body := ask(t, addr, "host:version")
	if string(body) != "0028" {
		t.Errorf("version = %q, want the upstream's own 0028", body)
	}
}

func TestUpstreamDownIsExplained(t *testing.T) {
	// Nothing is listening here.
	p := &Proxy{Upstream: "127.0.0.1:1"}
	addr := startProxy(t, p)
	status, body := ask(t, addr, "host:devices")
	if status != "FAIL" {
		t.Errorf("status = %q, want FAIL", status)
	}
	if !strings.Contains(string(body), "adb start-server") {
		t.Errorf("message %q does not say how to fix it", body)
	}
}

func TestStatusReport(t *testing.T) {
	p := &Proxy{Upstream: fakeServer(t, versionReply)}
	addr := startProxy(t, p)

	_, body := ask(t, addr, "radb:status")
	if !strings.Contains(string(body), "no client has tried") {
		t.Errorf("clean status = %q", body)
	}

	ask(t, addr, "host:kill")
	_, body = ask(t, addr, "radb:status")
	if !strings.Contains(string(body), "1 refused kill") {
		t.Errorf("status after a kill = %q", body)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	for _, s := range []string{"", "host:devices", strings.Repeat("x", 4096)} {
		var b strings.Builder
		if err := writeFrame(&b, []byte(s)); err != nil {
			t.Fatal(err)
		}
		got, err := readFrame(strings.NewReader(b.String()))
		if err != nil {
			t.Fatalf("round trip %d bytes: %v", len(s), err)
		}
		if string(got) != s {
			t.Errorf("round trip lost data at %d bytes", len(s))
		}
		if n, _ := strconv.ParseUint(b.String()[:4], 16, 32); int(n) != len(s) {
			t.Errorf("header says %d, payload is %d", n, len(s))
		}
	}
}

func TestBadFrameHeaderIsRejected(t *testing.T) {
	if _, err := readFrame(strings.NewReader("zzzz")); err == nil {
		t.Fatal("want an error for a non-hex length")
	}
}
