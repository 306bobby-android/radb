package fastboot

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/gousb"
)

// A bootloader exposes a vendor-specific interface with this class triple. adb
// uses the same class and subclass but protocol 1, so the protocol byte is what
// separates a device sitting in fastboot from one booted into Android.
const (
	ifClass    = gousb.ClassVendorSpec
	ifSubClass = 0x42
	ifProtocol = 0x03
)

// ErrNoDevice reports that no bootloader is currently attached over USB.
var ErrNoDevice = errors.New("no device in fastboot mode found on USB")

// Info describes a bootloader visible on USB.
type Info struct {
	Serial string
	Path   string
}

// Device is a claimed fastboot bootloader.
type Device struct {
	Serial string
	Path   string

	// Timeout bounds a single USB transfer. Erasing or flashing a large
	// partition legitimately takes minutes, so this wants to be generous.
	Timeout time.Duration

	ctx  *gousb.Context
	dev  *gousb.Device
	cfg  *gousb.Config
	intf *gousb.Interface
	in   *gousb.InEndpoint
	out  *gousb.OutEndpoint
}

// findInterface locates the bootloader interface within a device descriptor.
func findInterface(d *gousb.DeviceDesc) (cfgNum, ifNum, altNum int, ok bool) {
	for _, cfg := range d.Configs {
		for _, iff := range cfg.Interfaces {
			for _, alt := range iff.AltSettings {
				if alt.Class == ifClass && alt.SubClass == ifSubClass && alt.Protocol == ifProtocol {
					return cfg.Number, iff.Number, alt.Alternate, true
				}
			}
		}
	}
	return 0, 0, 0, false
}

// devPath renders the physical bus/port path, which stays the same across
// reboots of a device left in the same socket.
func devPath(d *gousb.DeviceDesc) string {
	if len(d.Path) == 0 {
		return fmt.Sprintf("%d-%d", d.Bus, d.Address)
	}
	parts := make([]string, len(d.Path))
	for i, p := range d.Path {
		parts[i] = strconv.Itoa(p)
	}
	return fmt.Sprintf("%d-%s", d.Bus, strings.Join(parts, "."))
}

// openAll returns every attached bootloader, along with the context owning them.
func openAll() (*gousb.Context, []*gousb.Device, error) {
	ctx := gousb.NewContext()
	devs, err := ctx.OpenDevices(func(d *gousb.DeviceDesc) bool {
		_, _, _, ok := findInterface(d)
		return ok
	})
	// OpenDevices reports partial failures alongside the devices it did open;
	// only treat that as fatal when it left us with nothing.
	if err != nil && len(devs) == 0 {
		ctx.Close()
		return nil, nil, fmt.Errorf("enumerate usb: %w", err)
	}
	return ctx, devs, nil
}

// List returns every device currently in fastboot mode.
func List() ([]Info, error) {
	ctx, devs, err := openAll()
	if err != nil {
		return nil, err
	}
	defer ctx.Close()
	for _, d := range devs {
		defer d.Close()
	}

	out := make([]Info, 0, len(devs))
	for _, d := range devs {
		sn, err := d.SerialNumber()
		if err != nil {
			sn = "<serial unreadable>"
		}
		out = append(out, Info{Serial: sn, Path: devPath(d.Desc)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Serial < out[j].Serial })
	return out, nil
}

// Open claims the bootloader with the given serial, or the only one attached
// when serial is empty.
func Open(serial string) (*Device, error) {
	ctx, devs, err := openAll()
	if err != nil {
		return nil, err
	}

	var matches []*gousb.Device
	for _, d := range devs {
		if serial != "" {
			sn, err := d.SerialNumber()
			if err != nil || sn != serial {
				d.Close()
				continue
			}
		}
		matches = append(matches, d)
	}

	fail := func(err error) (*Device, error) {
		for _, d := range matches {
			d.Close()
		}
		ctx.Close()
		return nil, err
	}

	switch {
	case len(matches) == 0:
		if serial != "" {
			return fail(fmt.Errorf("%w with serial %q", ErrNoDevice, serial))
		}
		return fail(ErrNoDevice)
	case len(matches) > 1:
		return fail(fmt.Errorf("%d devices are in fastboot mode; choose one with -s SERIAL", len(matches)))
	}

	d := &Device{ctx: ctx, dev: matches[0]}
	if err := d.claim(); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// claim selects the configuration and takes the two bulk endpoints.
func (d *Device) claim() error {
	cfgNum, ifNum, altNum, ok := findInterface(d.dev.Desc)
	if !ok {
		return errors.New("device stopped advertising a fastboot interface")
	}
	// Nothing in the kernel binds a vendor-specific interface, but detach
	// whatever might have rather than failing the claim outright.
	if err := d.dev.SetAutoDetach(true); err != nil {
		return fmt.Errorf("set auto detach: %w", err)
	}

	cfg, err := d.dev.Config(cfgNum)
	if err != nil {
		return fmt.Errorf("select configuration %d: %w", cfgNum, err)
	}
	d.cfg = cfg

	intf, err := cfg.Interface(ifNum, altNum)
	if err != nil {
		return fmt.Errorf("claim interface %d.%d: %w", ifNum, altNum, err)
	}
	d.intf = intf

	inNum, outNum := -1, -1
	for _, ep := range intf.Setting.Endpoints {
		if ep.TransferType != gousb.TransferTypeBulk {
			continue
		}
		if ep.Direction == gousb.EndpointDirectionIn {
			inNum = ep.Number
		} else {
			outNum = ep.Number
		}
	}
	if inNum < 0 || outNum < 0 {
		return fmt.Errorf("fastboot interface has no bulk endpoint pair (in=%d out=%d)", inNum, outNum)
	}
	if d.in, err = intf.InEndpoint(inNum); err != nil {
		return fmt.Errorf("open bulk IN endpoint %d: %w", inNum, err)
	}
	if d.out, err = intf.OutEndpoint(outNum); err != nil {
		return fmt.Errorf("open bulk OUT endpoint %d: %w", outNum, err)
	}

	d.Serial, _ = d.dev.SerialNumber()
	d.Path = devPath(d.dev.Desc)
	return nil
}

// Close releases the interface, the device and the libusb context.
func (d *Device) Close() {
	if d.intf != nil {
		d.intf.Close()
	}
	if d.cfg != nil {
		d.cfg.Close()
	}
	if d.dev != nil {
		d.dev.Close()
	}
	if d.ctx != nil {
		d.ctx.Close()
	}
}

// InPacketSize is the bulk IN endpoint's maximum packet size. Reads want to be
// a multiple of it: libusb fails a transfer with an overflow error if the
// device sends a packet bigger than the room left in the buffer.
func (d *Device) InPacketSize() int { return d.in.Desc.MaxPacketSize }

// deadline bounds one transfer. The cancel func must always be called, so each
// transfer gets its own rather than sharing a session-wide context.
func (d *Device) deadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if d.Timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d.Timeout)
}

// Send writes b to the bulk OUT endpoint, looping until it is all committed.
func (d *Device) Send(ctx context.Context, b []byte) error {
	for len(b) > 0 {
		n, err := func() (int, error) {
			tctx, cancel := d.deadline(ctx)
			defer cancel()
			return d.out.WriteContext(tctx, b)
		}()
		if err != nil {
			return fmt.Errorf("usb write: %w", err)
		}
		b = b[n:]
	}
	return nil
}

// Recv reads one bulk IN transfer into b and reports how much arrived.
func (d *Device) Recv(ctx context.Context, b []byte) (int, error) {
	tctx, cancel := d.deadline(ctx)
	defer cancel()
	n, err := d.in.ReadContext(tctx, b)
	if err != nil {
		return n, fmt.Errorf("usb read: %w", err)
	}
	return n, nil
}
