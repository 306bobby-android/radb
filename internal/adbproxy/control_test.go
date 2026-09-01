package adbproxy

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// logged runs a proxy whose log goes into a buffer the test can read.
func logged(t *testing.T, p *Proxy) (string, func() string) {
	t.Helper()
	var mu sync.Mutex
	buf := &bytes.Buffer{}
	p.Log = slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: buf}, nil))
	return startProxy(t, p), func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// `adb connect` is the one stock adb command that carries a word of our
// choosing to the server and prints the answer, which makes it the only way to
// stop radb without putting a tool on the remote machine.
func TestConnectStopsRadb(t *testing.T) {
	stopped := make(chan string, 1)
	p := &Proxy{
		Upstream: fakeServer(t, versionReply),
		Shutdown: func(reason string) { stopped <- reason },
	}
	addr := startProxy(t, p)

	status, body := ask(t, addr, "host:connect:"+ShutdownHost)
	if status != "OKAY" {
		t.Errorf("status = %q; adb only prints the body of an OKAY", status)
	}
	if !strings.Contains(string(body), "radb is stopping") {
		t.Errorf("reply %q does not say what happened", body)
	}
	select {
	case reason := <-stopped:
		if !strings.Contains(reason, "127.0.0.1") {
			t.Errorf("reason %q does not name who asked", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("radb was not asked to stop")
	}
}

// The adb server appends a default port to a host given without one, so the
// request can arrive either way.
func TestShutdownAddressIsRecognisedWithOrWithoutAPort(t *testing.T) {
	yes := []string{
		"host:connect:" + ShutdownHost,
		"host:connect:" + ShutdownHost + ":5555",
		"host:disconnect:" + ShutdownHost,
	}
	no := []string{
		"host:connect:192.168.1.5:5555",
		"host:connect:radb-shutdown.example.com",
		"host:devices",
		"radb:status",
	}
	for _, s := range yes {
		if !isShutdownRequest(s) {
			t.Errorf("%q was not taken as a shutdown request", s)
		}
	}
	for _, s := range no {
		if isShutdownRequest(s) {
			t.Errorf("%q was taken as a shutdown request", s)
		}
	}
}

func TestShutdownServiceForScripts(t *testing.T) {
	stopped := make(chan string, 1)
	p := &Proxy{
		Upstream: fakeServer(t, versionReply),
		Shutdown: func(reason string) { stopped <- reason },
	}
	addr := startProxy(t, p)
	if status, _ := ask(t, addr, "radb:shutdown"); status != "OKAY" {
		t.Errorf("status = %q", status)
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("radb was not asked to stop")
	}
}

func TestShutdownCanBeWithheld(t *testing.T) {
	p := &Proxy{Upstream: fakeServer(t, versionReply)} // no Shutdown
	addr := startProxy(t, p)

	status, body := ask(t, addr, "host:connect:"+ShutdownHost)
	if status != "OKAY" {
		t.Errorf("status = %q, want the refusal to be printable", status)
	}
	if !strings.Contains(string(body), "-remote-shutdown=false") {
		t.Errorf("reply %q does not say why nothing happened", body)
	}
}

// fakeTransportServer answers a transport switch and then the service that
// follows it on the same connection, which is what a real adb server does.
func fakeTransportServer(t *testing.T, saw chan<- string) string {
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
				for {
					svc, err := readFrame(c)
					if err != nil {
						return
					}
					saw <- string(svc)
					io.WriteString(c, "OKAY")
					if !isTransport(string(svc)) {
						io.WriteString(c, "hello\n")
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// host:transport-any says nothing about what the client is doing. The service
// that follows it is the command itself, and that is the one worth a log line.
func TestTransportCommandIsReported(t *testing.T) {
	saw := make(chan string, 4)
	p := &Proxy{Upstream: fakeTransportServer(t, saw)}
	addr, logs := logged(t, p)

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	writeFrame(c, []byte("host:transport-any"))
	var status [4]byte
	if _, err := io.ReadFull(c, status[:]); err != nil || string(status[:]) != "OKAY" {
		t.Fatalf("transport status = %q %v", status[:], err)
	}
	writeFrame(c, []byte("shell:getprop ro.product.model"))
	if _, err := io.ReadFull(c, status[:]); err != nil || string(status[:]) != "OKAY" {
		t.Fatalf("command status = %q %v", status[:], err)
	}
	body := make([]byte, 6)
	if _, err := io.ReadFull(c, body); err != nil {
		t.Fatalf("reading the command output: %v", err)
	}
	if string(body) != "hello\n" {
		t.Errorf("output = %q; the splice altered the stream", body)
	}

	// Both services must have reached the server untouched.
	for _, want := range []string{"host:transport-any", "shell:getprop ro.product.model"} {
		select {
		case got := <-saw:
			if got != want {
				t.Errorf("server saw %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("the server never saw %q", want)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs(), "getprop ro.product.model") {
		if time.Now().After(deadline) {
			t.Fatalf("the command was never logged:\n%s", logs())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDeviceComingAndGoingIsLogged(t *testing.T) {
	var buf bytes.Buffer
	p := &Proxy{Log: slog.New(slog.NewTextHandler(&buf, nil))}

	p.update(parseDeviceList("0A021FDD4005CG\tdevice\n"))
	p.update(parseDeviceList("0A021FDD4005CG\tunauthorized\n"))
	p.update(parseDeviceList(""))

	out := buf.String()
	for _, want := range []string{
		"adb device present",
		"adb device changed state",
		"adb device disconnected",
		"0A021FDD4005CG",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log does not mention %q:\n%s", want, out)
		}
	}
	if devs := p.Devices(); len(devs) != 0 {
		t.Errorf("devices after everything went away = %v", devs)
	}
}
