package core

// Picking the module's interface out of the host's list is reasoning about
// the data, not about any tool's output format, which is why it sits here
// while the ifconfig and netstat readers stay on the platform side.

func sessionTrafficFromCounters(current, baseline NetworkByteCounters) (rx, tx, total uint64) {
	rx = current.RX - baseline.RX
	tx = current.TX - baseline.TX
	return rx, tx, rx + tx
}

// hasUSBNetworkInterface reports whether the module has an interface of its own
// that is up. Any other interface being up says nothing about the module.
func hasUSBNetworkInterface(interfaces []macNetInterface, moduleInterface string) bool {
	return selectUSBTrafficInterface(interfaces, moduleInterface) != ""
}

// selectUSBTrafficInterface returns the module's interface, and only that.
//
// It used to fall back to "any active ethernet that is not en0", which reported
// whatever NIC happened to be up — on a machine where en0 is wired and Wi-Fi is
// en1, that meant presenting the user's Wi-Fi traffic as the module's. Reporting
// nothing is the correct answer when the module has no interface.
func selectUSBTrafficInterface(interfaces []macNetInterface, moduleInterface string) string {
	if moduleInterface == "" {
		return ""
	}
	for _, item := range interfaces {
		if item.Name == moduleInterface && item.Status == "active" {
			return item.Name
		}
	}
	return ""
}
