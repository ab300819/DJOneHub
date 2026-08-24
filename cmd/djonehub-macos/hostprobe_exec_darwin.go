//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ab300819/DJOneHub/internal/modem"
)

func discoverATPort() (string, error) {
	var ports []string
	for _, pattern := range []string{
		"/dev/cu.usbmodem*",
		"/dev/cu.usbserial*",
		"/dev/cu.wchusbserial*",
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return "", err
		}
		ports = append(ports, matches...)
	}

	sort.SliceStable(ports, func(i, j int) bool {
		return portScore(ports[i]) > portScore(ports[j])
	})
	var attempted []string
	for _, port := range ports {
		attempted = append(attempted, port)
		if _, err := modem.ProbeIMEICached(port, 2*time.Second); err == nil {
			return port, nil
		}
	}
	if len(attempted) == 0 {
		return "", errors.New("no Quectel/DJI USB serial ports found; pass -port /dev/cu.* explicitly")
	}
	return "", fmt.Errorf("no AT-capable port found among %s", strings.Join(attempted, ", "))
}

func discoverDJIUSBDevice() *usbDeviceStatus {
	out, err := exec.Command("ioreg", "-r", "-c", "IOUSBHostInterface", "-l", "-w", "0").Output()
	if err != nil {
		return nil
	}

	var device *usbDeviceStatus
	for _, block := range strings.Split(string(out), "\n\n") {
		vendorID, okVendor := intProperty(block, "idVendor")
		productID, okProduct := intProperty(block, "idProduct")
		if !okVendor || !okProduct || (vendorID != 0x2ca3 && vendorID != 0x2c7c) {
			continue
		}
		if device == nil {
			device = &usbDeviceStatus{
				Product:    stringProperty(block, "USB Product Name"),
				Vendor:     stringProperty(block, "USB Vendor Name"),
				VendorID:   fmt.Sprintf("%04x", vendorID),
				ProductID:  fmt.Sprintf("%04x", productID),
				LocationID: formatHexProperty(block, "locationID"),
				Speed:      usbSpeedName(intPropertyOrZero(block, "USBSpeed")),
				Mode:       "vendor-specific USB mode",
			}
			if strings.TrimSpace(device.Product) == "" {
				device.Product = "DJI 4G Module"
			}
			if strings.TrimSpace(device.Vendor) == "" {
				device.Vendor = "DJI"
			}
		}
		ifaceNumber, okIface := intProperty(block, "bInterfaceNumber")
		if !okIface {
			continue
		}
		iface := usbInterfaceStatus{
			Number:    ifaceNumber,
			Class:     intPropertyOrZero(block, "bInterfaceClass"),
			Subclass:  intPropertyOrZero(block, "bInterfaceSubClass"),
			Protocol:  intPropertyOrZero(block, "bInterfaceProtocol"),
			Endpoints: intPropertyOrZero(block, "bNumEndpoints"),
		}
		device.Interfaces = append(device.Interfaces, iface)
	}
	if device == nil {
		return nil
	}
	sort.SliceStable(device.Interfaces, func(i, j int) bool {
		return device.Interfaces[i].Number < device.Interfaces[j].Number
	})
	if allVendorSpecific(device.Interfaces) {
		device.Mode = "vendor-specific QMI/diagnostic mode"
	}
	return device
}

func discoverMacNetworkInterfaces() []macNetInterface {
	out, err := exec.Command("ifconfig").Output()
	if err != nil {
		return nil
	}
	var interfaces []macNetInterface
	for _, block := range splitIfconfigBlocks(string(out)) {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		lines := strings.Split(block, "\n")
		if len(lines) == 0 {
			continue
		}
		fields := strings.Fields(lines[0])
		if len(fields) == 0 {
			continue
		}
		name := strings.TrimSuffix(fields[0], ":")
		if name == "" || strings.HasPrefix(name, "lo") || strings.HasPrefix(name, "utun") {
			continue
		}
		item := macNetInterface{Name: name, Status: "unknown", Kind: classifyMacInterfaceName(name)}
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "status:") {
				item.Status = strings.TrimSpace(strings.TrimPrefix(line, "status:"))
			}
			if strings.HasPrefix(line, "inet ") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					item.IPv4 = fields[1]
				}
			}
		}
		interfaces = append(interfaces, item)
	}
	return interfaces
}

func discoverMacDefaultRoute() macDefaultRoute {
	out, err := exec.Command("route", "-n", "get", "default").Output()
	if err != nil {
		return macDefaultRoute{}
	}
	var route macDefaultRoute
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "gateway:") {
			route.Gateway = strings.TrimSpace(strings.TrimPrefix(line, "gateway:"))
		}
		if strings.HasPrefix(line, "interface:") {
			route.Interface = strings.TrimSpace(strings.TrimPrefix(line, "interface:"))
		}
	}
	return route
}

func discoverMacInterfaceCounters() (map[string]networkByteCounters, error) {
	out, err := exec.Command("netstat", "-ibn").Output()
	if err != nil {
		return nil, err
	}
	return parseMacInterfaceCounters(string(out)), nil
}

// discoverModuleNetworkInterface asks IOKit which network interface belongs to
// the module. ioreg prints one blank-line-separated block per USB device and
// includes that device's whole subtree, so the ECM driver's BSD name sits in
// the same block as the module's own idVendor.
//
// This is the only sound way to answer the question. Interface names are handed
// out in enumeration order, so "en0 is Wi-Fi and any other en* is the module"
// is wrong on any machine with a second NIC — including ones where en0 is wired
// and Wi-Fi landed on en1.
func discoverModuleNetworkInterface() string {
	out, err := exec.Command("ioreg", "-r", "-c", "IOUSBHostDevice", "-l", "-w", "0").Output()
	if err != nil {
		return ""
	}
	return parseModuleNetworkInterface(string(out))
}
