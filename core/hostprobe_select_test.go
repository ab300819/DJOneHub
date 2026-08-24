package core

import (
	"testing"

	"github.com/ab300819/DJOneHub/internal/service"
)

func TestSelectUSBTrafficInterfaceTakesOnlyTheModulesOwnInterface(t *testing.T) {
	interfaces := []service.MacNetInterface{
		{Name: "en0", Kind: "ethernet", Status: "active", IPv4: "192.168.1.29"},
		{Name: "en1", Kind: "ethernet", Status: "active", IPv4: "10.0.0.7"},
		{Name: "en9", Kind: "ethernet", Status: "active", IPv4: "192.168.225.23"},
	}
	if got := selectUSBTrafficInterface(interfaces, "en9"); got != "en9" {
		t.Fatalf("selected interface = %q, want en9", got)
	}
}

// This is the case the old heuristic got wrong: several active ethernet
// interfaces, none of them the module. en0 being wired and Wi-Fi being en1 is an
// ordinary macOS layout, and the module's traffic view must not borrow either.
func TestSelectUSBTrafficInterfaceReportsNothingWithoutTheModule(t *testing.T) {
	interfaces := []service.MacNetInterface{
		{Name: "en0", Kind: "ethernet", Status: "active", IPv4: "192.168.1.29"},
		{Name: "en1", Kind: "ethernet", Status: "active", IPv4: "10.0.0.7"},
	}
	if got := selectUSBTrafficInterface(interfaces, ""); got != "" {
		t.Fatalf("selected interface = %q with no module present, want none", got)
	}
	if got := selectUSBTrafficInterface(interfaces, "en9"); got != "" {
		t.Fatalf("selected interface = %q for an absent en9, want none", got)
	}
}

// An interface the module created but that has not come up carries nothing.
func TestSelectUSBTrafficInterfaceRequiresTheModuleInterfaceToBeUp(t *testing.T) {
	interfaces := []service.MacNetInterface{{Name: "en9", Kind: "ethernet", Status: "inactive"}}
	if got := selectUSBTrafficInterface(interfaces, "en9"); got != "" {
		t.Fatalf("selected interface = %q for an inactive module NIC, want none", got)
	}
}

func TestSessionTrafficIsDownloadPlusUpload(t *testing.T) {
	rx, tx, total := sessionTrafficFromCounters(
		NetworkByteCounters{RX: 8192, TX: 4096},
		NetworkByteCounters{RX: 2048, TX: 1024},
	)
	if rx != 6144 || tx != 3072 || total != 9216 {
		t.Fatalf("session traffic = rx:%d tx:%d total:%d", rx, tx, total)
	}
}
