package main

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

	// NetworkInterfaces lists the machine's interfaces with their addresses.
	NetworkInterfaces() []macNetInterface

	// DefaultRoute reports where the machine currently sends traffic.
	DefaultRoute() macDefaultRoute

	// InterfaceCounters reports per-interface byte totals since boot.
	InterfaceCounters() (map[string]networkByteCounters, error)

	// ProcessFlows samples the flows currently on the wire for one protocol,
	// "tcp" or "udp".
	ProcessFlows(protocol string) ([]service.ActivityRecord, error)
}

// probe returns the host probe this app was built with. Tests construct &app{}
// directly and pass their own; leaving the field empty has to keep working, so
// the platform default fills in rather than panicking.
func (a *app) probe() HostProbe {
	if a.host != nil {
		return a.host
	}
	return defaultHostProbe()
}

// unsupportedHost is what a platform without ioreg, ifconfig, route and nettop
// reports: nothing. The network views then degrade to empty, which is the same
// shape macOS produces when no module is attached. It lives here rather than
// behind a build tag so that the tests can exercise it on any platform.
type unsupportedHost struct{}

func (unsupportedHost) USBDevice() *usbDeviceStatus { return nil }

func (unsupportedHost) NetworkInterfaces() []macNetInterface { return nil }

func (unsupportedHost) DefaultRoute() macDefaultRoute { return macDefaultRoute{} }

func (unsupportedHost) InterfaceCounters() (map[string]networkByteCounters, error) {
	return nil, errors.New("host interface counters are not available on this platform")
}

func (unsupportedHost) ProcessFlows(string) ([]service.ActivityRecord, error) {
	return nil, errors.New("host flow sampling is not available on this platform")
}

var _ HostProbe = unsupportedHost{}
