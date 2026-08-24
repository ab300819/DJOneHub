package core

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
	usb         *service.USBDevice
	moduleIface string
	interfaces  []service.MacNetInterface
	route       service.MacDefaultRoute
	counters    map[string]NetworkByteCounters
	countersErr error
	flows       map[string][]service.ActivityRecord
	flowsErr    error
}

func (f fakeHost) USBDevice() *service.USBDevice { return f.usb }

func (f fakeHost) ATPort() (string, error) { return "", errors.New("no serial port") }

func (f fakeHost) ModuleInterface() string { return f.moduleIface }

func (f fakeHost) NetworkInterfaces() []service.MacNetInterface { return f.interfaces }

func (f fakeHost) DefaultRoute() service.MacDefaultRoute { return f.route }

func (f fakeHost) InterfaceCounters() (map[string]NetworkByteCounters, error) {
	return f.counters, f.countersErr
}

func (f fakeHost) ProcessFlows(protocol string) ([]service.ActivityRecord, error) {
	if f.flowsErr != nil {
		return nil, f.flowsErr
	}
	return f.flows[protocol], nil
}

// ioregWithModule is trimmed from real `ioreg -r -c IOUSBHostDevice -l -w 0`
// output with the module attached: one blank-line-separated block per USB
// device, each carrying its whole subtree. The module's four vendor-specific
// interfaces come first, then the ECM control interface whose driver chain ends
// at the BSD name — which is why the name can be found in the same block as the
// module's own idVendor.

// moduleAttached is a Mac with the module up on en11 and the default route
// already pointed at it — the state the whole feature exists to produce.
func moduleAttached() fakeHost {
	return fakeHost{
		usb:         &service.USBDevice{Vendor: "Quectel", Product: "EG25-G"},
		moduleIface: "en11",
		interfaces: []service.MacNetInterface{
			{Name: "en0", Kind: "wifi", Status: "active", IPv4: "192.168.1.20"},
			{Name: "en11", Kind: "ethernet", Status: "active", IPv4: "192.168.225.34"},
		},
		route: service.MacDefaultRoute{Interface: "en11", Gateway: "192.168.225.1"},
	}
}

func TestCheck4GRouteRecognizesTheModuleAsTheDefaultExit(t *testing.T) {
	instance := &App{host: moduleAttached()}

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
	host.route = service.MacDefaultRoute{Interface: "en0", Gateway: "192.168.1.1"}
	if result := (&App{host: host}).Check4GRoute(); result.OK {
		t.Errorf("Check4GRoute() reported OK while the default exit was Wi-Fi: %+v", result)
	}

	host.route = service.MacDefaultRoute{}
	result := (&App{host: host}).Check4GRoute()
	if result.OK {
		t.Errorf("Check4GRoute() reported OK with no default route: %+v", result)
	}
	if result.Summary != "未读取到默认出口" {
		t.Errorf("Summary = %q, want the missing-route summary", result.Summary)
	}
}

func TestNetworkTrafficCountsFromTheFirstSampleAsBaseline(t *testing.T) {
	host := moduleAttached()
	host.counters = map[string]NetworkByteCounters{"en11": {RX: 1000, TX: 400}}
	instance := &App{host: host}

	first := instance.NetworkTraffic()
	if !first.Available || first.Interface != "en11" {
		t.Fatalf("first sample = %+v, want en11 available", first)
	}
	if first.SessionTotal != 0 {
		t.Errorf("SessionTotal = %d on the first sample, want 0", first.SessionTotal)
	}

	host.counters = map[string]NetworkByteCounters{"en11": {RX: 1600, TX: 500}}
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
	host.counters = map[string]NetworkByteCounters{"en11": {RX: 5000, TX: 5000}}
	instance := &App{host: host}
	instance.NetworkTraffic()

	host.counters = map[string]NetworkByteCounters{"en11": {RX: 10, TX: 10}}
	instance.host = host
	after := instance.NetworkTraffic()
	if after.SessionTotal != 0 {
		t.Errorf("SessionTotal = %d after a counter reset, want 0", after.SessionTotal)
	}
}

func TestNetworkTrafficReportsAFailedCounterRead(t *testing.T) {
	host := moduleAttached()
	host.countersErr = errors.New("netstat is unavailable")
	snapshot := (&App{host: host}).NetworkTraffic()

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
	if conn := (&App{host: moduleAttached()}).LocalNetworkConnection(); conn == nil {
		t.Fatal("LocalNetworkConnection() = nil with the module up")
	} else if conn.Interface != "en11" || conn.IPv4 != "192.168.225.34" || !conn.IsDefault {
		t.Errorf("connection = %+v, want en11/192.168.225.34 as default", conn)
	}

	unplugged := moduleAttached()
	unplugged.usb = nil
	if conn := (&App{host: unplugged}).LocalNetworkConnection(); conn != nil {
		t.Errorf("LocalNetworkConnection() = %+v with no module attached, want nil", conn)
	}

	noInterface := moduleAttached()
	noInterface.interfaces = []service.MacNetInterface{{Name: "en0", Kind: "wifi", Status: "active"}}
	if conn := (&App{host: noInterface}).LocalNetworkConnection(); conn != nil {
		t.Errorf("LocalNetworkConnection() = %+v with only Wi-Fi present, want nil", conn)
	}
}

// With a VPN up the flows appear on the tunnel, not on the module's interface,
// so that is where the connection list is read from — while the module itself
// still has to be reported as carrying traffic.
func TestNetworkActivityReadsConnectionsFromTheTunnel(t *testing.T) {
	host := moduleAttached()
	host.interfaces = append(host.interfaces,
		service.MacNetInterface{Name: "utun4", Kind: "tunnel", Status: "active"})
	host.route = service.MacDefaultRoute{Interface: "utun4"}
	host.flows = map[string][]service.ActivityRecord{
		"tcp": {
			{Process: "Safari", Interface: "utun4", RXBytes: 900, TXBytes: 100},
			{Process: "kernel", Interface: "en11", RXBytes: 50, TXBytes: 50},
		},
		"udp": {{Process: "mDNSResponder", Interface: "utun4", RXBytes: 10, TXBytes: 10}},
	}
	snapshot := (&App{host: host}).NetworkActivity()

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
	snapshot := (&App{host: host}).NetworkActivity()

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
	snapshot := (&App{host: host}).NetworkActivity()

	if !snapshot.Available {
		t.Error("Available = false although the module and interface were found")
	}
	if len(snapshot.Connections) != 0 {
		t.Errorf("Connections = %+v, want none", snapshot.Connections)
	}
}

func TestNetworkDiagnosticReportsWhatTheHostSees(t *testing.T) {
	instance := &App{host: moduleAttached(), demo: true}

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
	instance := &App{host: UnsupportedHost{}}

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

// The bug this replaced: a second active ethernet interface that is not the
// module was accepted as proof the module was carrying traffic. On a Mac where
// en0 is wired and Wi-Fi is en1, that reported Wi-Fi as the 4G link.
func TestCheck4GRouteRejectsASecondNICThatIsNotTheModule(t *testing.T) {
	host := fakeHost{
		usb:         &service.USBDevice{Vendor: "Quectel", Product: "EG25-G"},
		moduleIface: "",
		interfaces: []service.MacNetInterface{
			{Name: "en0", Kind: "ethernet", Status: "active", IPv4: "192.168.1.29"},
			{Name: "en1", Kind: "ethernet", Status: "active", IPv4: "10.0.0.7"},
		},
		route: service.MacDefaultRoute{Interface: "en1", Gateway: "10.0.0.1"},
	}
	if result := (&App{host: host}).Check4GRoute(); result.OK {
		t.Errorf("Check4GRoute() = %+v, want not OK: en1 is Wi-Fi, not the module", result)
	}
}

func TestNetworkDiagnosticDoesNotClaimAUSBNetworkFromAnotherNIC(t *testing.T) {
	host := fakeHost{
		interfaces: []service.MacNetInterface{
			{Name: "en0", Kind: "ethernet", Status: "active"},
			{Name: "en1", Kind: "ethernet", Status: "active"},
		},
	}
	diag, err := (&App{host: host, demo: true}).NetworkDiagnostic()
	if err != nil {
		t.Fatalf("NetworkDiagnostic() error = %v", err)
	}
	if diag.USBNetworkPresent {
		t.Error("USBNetworkPresent = true with no module interface present")
	}
}
