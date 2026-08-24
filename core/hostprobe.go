package core

import (
	"errors"

	"github.com/ab300819/DJOneHub/internal/service"
)

// HostProbe is everything the core needs to know about the machine the module
// is plugged into: which USB device is present, what network interfaces exist,
// where the default route points, and how much traffic went through. On macOS
// that is answered by ioreg, ifconfig, route and nettop; on Android none of
// those exist and the implementation says so by returning nothing.
//
// The core never names a command-line tool. That is the whole point: the
// methods below are the only place a platform gets to be macOS-shaped.
type HostProbe interface {
	// USBDevice reports the attached module, or nil when none is present.
	USBDevice() *usbDeviceStatus

	// ATPort names a serial AT port to drive the module through, for hosts that
	// expose one. The USB transport is used when this finds nothing.
	ATPort() (string, error)

	// ModuleInterface names the network interface the module itself created,
	// or "" when the module is absent or has no network interface up. It is the
	// only trustworthy answer to "which interface is the module": interface
	// names are assigned in enumeration order and say nothing about hardware.
	ModuleInterface() string

	// NetworkInterfaces lists the machine's interfaces with their addresses.
	NetworkInterfaces() []macNetInterface

	// DefaultRoute reports where the machine currently sends traffic.
	DefaultRoute() macDefaultRoute

	// InterfaceCounters reports per-interface byte totals since boot.
	InterfaceCounters() (map[string]NetworkByteCounters, error)

	// ProcessFlows samples the flows currently on the wire for one protocol,
	// "tcp" or "udp".
	ProcessFlows(protocol string) ([]service.ActivityRecord, error)
}

// probe returns the host probe this app was built with. Every construction that
// runs for real injects one; what remains is a net under the tests that build
// &App{} and never touch the host, so an empty field reports an absent machine
// instead of panicking. It deliberately does not fall back to the platform
// default: a core that reaches for macOS on its own has no seam at all, and a
// forgotten injection should look like nothing rather than quietly work on one
// platform and break on the next.
func (a *App) probe() HostProbe {
	if a.host != nil {
		return a.host
	}
	return UnsupportedHost{}
}

// UnsupportedHost is what a platform without ioreg, ifconfig, route and nettop
// reports: nothing. The network views then degrade to empty, which is the same
// shape macOS produces when no module is attached. It lives here rather than
// behind a build tag so that the tests can exercise it on any platform.
type UnsupportedHost struct{}

func (UnsupportedHost) USBDevice() *usbDeviceStatus { return nil }

func (UnsupportedHost) ATPort() (string, error) {
	return "", errors.New("serial AT port discovery is not available on this platform")
}

func (UnsupportedHost) ModuleInterface() string { return "" }

func (UnsupportedHost) NetworkInterfaces() []macNetInterface { return nil }

func (UnsupportedHost) DefaultRoute() macDefaultRoute { return macDefaultRoute{} }

func (UnsupportedHost) InterfaceCounters() (map[string]NetworkByteCounters, error) {
	return nil, errors.New("host interface counters are not available on this platform")
}

func (UnsupportedHost) ProcessFlows(string) ([]service.ActivityRecord, error) {
	return nil, errors.New("host flow sampling is not available on this platform")
}

var _ HostProbe = UnsupportedHost{}
