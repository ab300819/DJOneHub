package main

import (
	"testing"
)

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
