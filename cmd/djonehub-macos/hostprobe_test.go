package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/ab300819/DJOneHub/internal/service"
)

// fakeHost describes a machine instead of asking one. Until HostProbe existed
// these five methods reported whatever the developer's Mac happened to have
// plugged in, so none of the branches below could be pinned down.
type fakeHost struct {
	usb         *usbDeviceStatus
	moduleIface string
	interfaces  []macNetInterface
	route       macDefaultRoute
	counters    map[string]networkByteCounters
	countersErr error
	flows       map[string][]service.ActivityRecord
	flowsErr    error
}

func (f fakeHost) USBDevice() *usbDeviceStatus          { return f.usb }
func (f fakeHost) ATPort() (string, error)              { return "", errors.New("no serial port") }
func (f fakeHost) ModuleInterface() string              { return f.moduleIface }
func (f fakeHost) NetworkInterfaces() []macNetInterface { return f.interfaces }
func (f fakeHost) DefaultRoute() macDefaultRoute        { return f.route }

func (f fakeHost) InterfaceCounters() (map[string]networkByteCounters, error) {
	return f.counters, f.countersErr
}

func (f fakeHost) ProcessFlows(protocol string) ([]service.ActivityRecord, error) {
	if f.flowsErr != nil {
		return nil, f.flowsErr
	}
	return f.flows[protocol], nil
}

// moduleAttached is a Mac with the module up on en11 and the default route
// already pointed at it — the state the whole feature exists to produce.
func moduleAttached() fakeHost {
	return fakeHost{
		usb:         &usbDeviceStatus{Vendor: "Quectel", Product: "EG25-G"},
		moduleIface: "en11",
		interfaces: []macNetInterface{
			{Name: "en0", Kind: "wifi", Status: "active", IPv4: "192.168.1.20"},
			{Name: "en11", Kind: "ethernet", Status: "active", IPv4: "192.168.225.34"},
		},
		route: macDefaultRoute{Interface: "en11", Gateway: "192.168.225.1"},
	}
}

func TestCheck4GRouteRecognizesTheModuleAsTheDefaultExit(t *testing.T) {
	instance := &app{host: moduleAttached()}

	result := instance.Check4GRoute()
	if !result.OK {
		t.Fatalf("Check4GRoute() = %+v, want OK with the module as default exit", result)
	}
	if want := "192.168.225.34"; !strings.Contains(result.Detail, want) {
		t.Errorf("Detail = %q, want it to name the module's IP %s", result.Detail, want)
	}
}

// en0 is Wi-Fi. Even when it is the default route and looks healthy, it is not
// the module, and the check has to say so.
func TestCheck4GRouteRejectsWiFiAndAMissingRoute(t *testing.T) {
	host := moduleAttached()
	host.route = macDefaultRoute{Interface: "en0", Gateway: "192.168.1.1"}
	if result := (&app{host: host}).Check4GRoute(); result.OK {
		t.Errorf("Check4GRoute() reported OK while the default exit was Wi-Fi: %+v", result)
	}

	host.route = macDefaultRoute{}
	result := (&app{host: host}).Check4GRoute()
	if result.OK {
		t.Errorf("Check4GRoute() reported OK with no default route: %+v", result)
	}
	if result.Summary != "未读取到默认出口" {
		t.Errorf("Summary = %q, want the missing-route summary", result.Summary)
	}
}

func TestNetworkTrafficCountsFromTheFirstSampleAsBaseline(t *testing.T) {
	host := moduleAttached()
	host.counters = map[string]networkByteCounters{"en11": {RX: 1000, TX: 400}}
	instance := &app{host: host}

	first := instance.NetworkTraffic()
	if !first.Available || first.Interface != "en11" {
		t.Fatalf("first sample = %+v, want en11 available", first)
	}
	if first.SessionTotal != 0 {
		t.Errorf("SessionTotal = %d on the first sample, want 0", first.SessionTotal)
	}

	host.counters = map[string]networkByteCounters{"en11": {RX: 1600, TX: 500}}
	instance.host = host
	second := instance.NetworkTraffic()
	if second.SessionRX != 600 || second.SessionTX != 100 || second.SessionTotal != 700 {
		t.Errorf("session counters = (%d, %d, %d), want (600, 100, 700)",
			second.SessionRX, second.SessionTX, second.SessionTotal)
	}
	if second.RXBytes != 1600 {
		t.Errorf("RXBytes = %d, want the absolute counter 1600", second.RXBytes)
	}
}

// A module that reboots resets its counters. Subtracting the old baseline would
// underflow, so the baseline has to be re-taken.
func TestNetworkTrafficRebaselinesWhenCountersGoBackwards(t *testing.T) {
	host := moduleAttached()
	host.counters = map[string]networkByteCounters{"en11": {RX: 5000, TX: 5000}}
	instance := &app{host: host}
	instance.NetworkTraffic()

	host.counters = map[string]networkByteCounters{"en11": {RX: 10, TX: 10}}
	instance.host = host
	after := instance.NetworkTraffic()
	if after.SessionTotal != 0 {
		t.Errorf("SessionTotal = %d after a counter reset, want 0", after.SessionTotal)
	}
}

func TestNetworkTrafficReportsAFailedCounterRead(t *testing.T) {
	host := moduleAttached()
	host.countersErr = errors.New("netstat is unavailable")
	snapshot := (&app{host: host}).NetworkTraffic()

	if snapshot.Available {
		t.Error("Available = true although the counters could not be read")
	}
	if snapshot.Interface != "en11" {
		t.Errorf("Interface = %q, want the interface to still be named", snapshot.Interface)
	}
	if snapshot.Error == "" {
		t.Error("Error is empty; the caller has nothing to show")
	}
}

func TestLocalNetworkConnectionNeedsBothAModuleAndAnInterface(t *testing.T) {
	if conn := (&app{host: moduleAttached()}).LocalNetworkConnection(); conn == nil {
		t.Fatal("LocalNetworkConnection() = nil with the module up")
	} else if conn.Interface != "en11" || conn.IPv4 != "192.168.225.34" || !conn.IsDefault {
		t.Errorf("connection = %+v, want en11/192.168.225.34 as default", conn)
	}

	unplugged := moduleAttached()
	unplugged.usb = nil
	if conn := (&app{host: unplugged}).LocalNetworkConnection(); conn != nil {
		t.Errorf("LocalNetworkConnection() = %+v with no module attached, want nil", conn)
	}

	noInterface := moduleAttached()
	noInterface.interfaces = []macNetInterface{{Name: "en0", Kind: "wifi", Status: "active"}}
	if conn := (&app{host: noInterface}).LocalNetworkConnection(); conn != nil {
		t.Errorf("LocalNetworkConnection() = %+v with only Wi-Fi present, want nil", conn)
	}
}

// With a VPN up the flows appear on the tunnel, not on the module's interface,
// so that is where the connection list is read from — while the module itself
// still has to be reported as carrying traffic.
func TestNetworkActivityReadsConnectionsFromTheTunnel(t *testing.T) {
	host := moduleAttached()
	host.interfaces = append(host.interfaces,
		macNetInterface{Name: "utun4", Kind: "tunnel", Status: "active"})
	host.route = macDefaultRoute{Interface: "utun4"}
	host.flows = map[string][]service.ActivityRecord{
		"tcp": {
			{Process: "Safari", Interface: "utun4", RXBytes: 900, TXBytes: 100},
			{Process: "kernel", Interface: "en11", RXBytes: 50, TXBytes: 50},
		},
		"udp": {{Process: "mDNSResponder", Interface: "utun4", RXBytes: 10, TXBytes: 10}},
	}
	snapshot := (&app{host: host}).NetworkActivity()

	if !snapshot.Available {
		t.Fatal("Available = false with the module up")
	}
	if snapshot.PhysicalInterface != "en11" || snapshot.TunnelInterface != "utun4" {
		t.Errorf("physical=%q tunnel=%q, want en11/utun4",
			snapshot.PhysicalInterface, snapshot.TunnelInterface)
	}
	if !snapshot.PhysicalActive {
		t.Error("PhysicalActive = false although a flow was seen on en11")
	}
	if len(snapshot.Connections) != 2 {
		t.Fatalf("Connections = %d, want the 2 tunnel flows", len(snapshot.Connections))
	}
	if snapshot.Connections[0].Process != "Safari" {
		t.Errorf("Connections are not sorted by volume: %+v", snapshot.Connections)
	}
}

// The sampler reports every process on the interface, not just the module's, so
// the snapshot is capped.
func TestNetworkActivityCapsTheConnectionList(t *testing.T) {
	host := moduleAttached()
	var many []service.ActivityRecord
	for i := range 20 {
		many = append(many, service.ActivityRecord{
			Process:   "proc",
			Interface: "en11",
			RXBytes:   uint64(i),
		})
	}
	host.flows = map[string][]service.ActivityRecord{"tcp": many}
	snapshot := (&app{host: host}).NetworkActivity()

	if len(snapshot.Connections) != maxActivityConnections {
		t.Errorf("Connections = %d, want the cap of %d",
			len(snapshot.Connections), maxActivityConnections)
	}
	if snapshot.Connections[0].RXBytes != 19 {
		t.Errorf("the cap dropped the heaviest flow: got %d", snapshot.Connections[0].RXBytes)
	}
}

// A sampler that cannot run must not take the rest of the snapshot with it.
func TestNetworkActivitySurvivesAFailedSampler(t *testing.T) {
	host := moduleAttached()
	host.flowsErr = errors.New("nettop is unavailable")
	snapshot := (&app{host: host}).NetworkActivity()

	if !snapshot.Available {
		t.Error("Available = false although the module and interface were found")
	}
	if len(snapshot.Connections) != 0 {
		t.Errorf("Connections = %+v, want none", snapshot.Connections)
	}
}

func TestNetworkDiagnosticReportsWhatTheHostSees(t *testing.T) {
	instance := &app{host: moduleAttached(), demo: true}

	diag, err := instance.NetworkDiagnostic()
	if err != nil {
		t.Fatalf("NetworkDiagnostic() error = %v", err)
	}
	if len(diag.MacInterfaces) != 2 {
		t.Errorf("MacInterfaces = %d, want the 2 the host reported", len(diag.MacInterfaces))
	}
	if diag.DefaultRoute.Interface != "en11" {
		t.Errorf("DefaultRoute = %+v, want en11", diag.DefaultRoute)
	}
	if !diag.USBNetworkPresent {
		t.Error("USBNetworkPresent = false although en11 is an active non-en0 ethernet")
	}
}

// The platform that has none of these tools has to degrade, not fail.
func TestUnsupportedHostDegradesInsteadOfFailing(t *testing.T) {
	instance := &app{host: unsupportedHost{}}

	if conn := instance.LocalNetworkConnection(); conn != nil {
		t.Errorf("LocalNetworkConnection() = %+v, want nil", conn)
	}
	if snapshot := instance.NetworkActivity(); snapshot.Available {
		t.Error("NetworkActivity reported Available on a host with no probes")
	}
	snapshot := instance.NetworkTraffic()
	if snapshot.Available || snapshot.Interface != "" {
		t.Errorf("NetworkTraffic() = %+v, want an empty snapshot", snapshot)
	}
	if result := instance.Check4GRoute(); result.OK {
		t.Errorf("Check4GRoute() = %+v, want not OK", result)
	}
}

// ioregWithModule is trimmed from real `ioreg -r -c IOUSBHostDevice -l -w 0`
// output with the module attached: one blank-line-separated block per USB
// device, each carrying its whole subtree. The module's four vendor-specific
// interfaces come first, then the ECM control interface whose driver chain ends
// at the BSD name — which is why the name can be found in the same block as the
// module's own idVendor.
const ioregWithModule = `+-o USB2 Hub@02100000  <class IOUSBHostDevice, id 0x100000a88, registered, matched, active, busy 0 (475 ms), retain 37>
  |   "idProduct" = 10775
  |   "idVendor" = 1452

+-o Baiwang@00100000  <class IOUSBHostDevice, id 0x1000ecaa1, registered, matched, active, busy 0 (133 ms), retain 122>
  |   "idProduct" = 293
  |   "idVendor" = 11388
  |   "iSerialNumber" = 0
  |   "USB Product Name" = "Baiwang"
  +-o IOUSBHostInterface@0  <class IOUSBHostInterface, id 0x1000ecaa6, registered, matched, active, busy 0 (15 ms), retain 7>
  |     "bInterfaceNumber" = 0
  |     "bInterfaceClass" = 255
  +-o CDC Ethernet Control Model (ECM)@4  <class IOUSBHostInterface, id 0x1000ecaaa, registered, matched, active, busy 0 (108 ms), retain 11>
  | |   "bInterfaceClass" = 2
  | |   "bInterfaceNumber" = 4
  | +-o AppleUserECM  <class IOUserNetworkEthernet, id 0x1000ecab0, registered, matched, active, busy 0 (8 ms), retain 21>
  |   |   "bInterfaceClass" = 2
  |   +-o IOSkywalkLegacyEthernet  <class IOSkywalkLegacyEthernet, id 0x1000ecabe, !registered, !matched, active, busy 0 (5 ms), retain 8>
  |   | +-o en11  <class IOSkywalkLegacyEthernetInterface, id 0x1000ecac0, registered, matched, active, busy 0 (5 ms), retain 10>
  |   |   |   "BSD Name" = "en11"
  +-o IOUSBHostInterface@5  <class IOUSBHostInterface, id 0x1000ecaac, registered, matched, active, busy 0 (91 ms), retain 12>
  | |   "bInterfaceClass" = 10
  | |   "bInterfaceNumber" = 5

+-o USB 2.0 BILLBOARD@01100000  <class IOUSBHostDevice, id 0x100000a95, registered, matched, active, busy 0 (389 ms), retain 71>
  |   "idProduct" = 20549
  |   "idVendor" = 1155
`

func TestParseModuleNetworkInterfaceFindsTheModulesOwnNIC(t *testing.T) {
	if got := parseModuleNetworkInterface(ioregWithModule); got != "en11" {
		t.Fatalf("parseModuleNetworkInterface() = %q, want en11", got)
	}
}

// Other USB devices on the bus have interfaces of their own. None of them is
// the answer.
func TestParseModuleNetworkInterfaceIgnoresOtherDevices(t *testing.T) {
	withoutModule := `+-o iPhone@03100000  <class IOUSBHostDevice, id 0x1000cc529, registered, matched, active, busy 0, retain 51>
  |   "idVendor" = 1452
  | +-o en5  <class IOEthernetInterface, id 0x1000cc600, registered, matched, active, busy 0, retain 6>
  | |   "BSD Name" = "en5"
`
	if got := parseModuleNetworkInterface(withoutModule); got != "" {
		t.Fatalf("parseModuleNetworkInterface() = %q for a bus without the module, want none", got)
	}
}

// The module enumerates before its network interface comes up, and in QMI mode
// it never gets one at all.
func TestParseModuleNetworkInterfaceReportsNothingBeforeECMBinds(t *testing.T) {
	qmiMode := `+-o Baiwang@01100000  <class IOUSBHostDevice, id 0x1000cd001, registered, matched, active, busy 0, retain 44>
  |   "idVendor" = 11388
  | +-o IOUSBHostInterface@4  <class IOUSBHostInterface, id 0x1000cd040, registered, matched, active, busy 0, retain 9>
  | |   "bInterfaceClass" = 255
  | |   "bInterfaceNumber" = 4
`
	if got := parseModuleNetworkInterface(qmiMode); got != "" {
		t.Fatalf("parseModuleNetworkInterface() = %q with no ECM interface, want none", got)
	}
}

// The bug this replaced: a second active ethernet interface that is not the
// module was accepted as proof the module was carrying traffic. On a Mac where
// en0 is wired and Wi-Fi is en1, that reported Wi-Fi as the 4G link.
func TestCheck4GRouteRejectsASecondNICThatIsNotTheModule(t *testing.T) {
	host := fakeHost{
		usb:         &usbDeviceStatus{Vendor: "Quectel", Product: "EG25-G"},
		moduleIface: "",
		interfaces: []macNetInterface{
			{Name: "en0", Kind: "ethernet", Status: "active", IPv4: "192.168.1.29"},
			{Name: "en1", Kind: "ethernet", Status: "active", IPv4: "10.0.0.7"},
		},
		route: macDefaultRoute{Interface: "en1", Gateway: "10.0.0.1"},
	}
	if result := (&app{host: host}).Check4GRoute(); result.OK {
		t.Errorf("Check4GRoute() = %+v, want not OK: en1 is Wi-Fi, not the module", result)
	}
}

func TestNetworkDiagnosticDoesNotClaimAUSBNetworkFromAnotherNIC(t *testing.T) {
	host := fakeHost{
		interfaces: []macNetInterface{
			{Name: "en0", Kind: "ethernet", Status: "active"},
			{Name: "en1", Kind: "ethernet", Status: "active"},
		},
	}
	diag, err := (&app{host: host, demo: true}).NetworkDiagnostic()
	if err != nil {
		t.Fatalf("NetworkDiagnostic() error = %v", err)
	}
	if diag.USBNetworkPresent {
		t.Error("USBNetworkPresent = true with no module interface present")
	}
}
