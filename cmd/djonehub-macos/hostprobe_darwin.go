//go:build darwin

package main

import (
	"os/exec"

	"github.com/ab300819/DJOneHub/internal/service"
)

// darwinHost answers HostProbe by running the tools macOS ships with. The
// discovery and parsing functions it delegates to still live in main.go; the
// package move puts them here.
type darwinHost struct{}

func defaultHostProbe() HostProbe { return darwinHost{} }

func (darwinHost) USBDevice() *usbDeviceStatus { return discoverDJIUSBDevice() }

func (darwinHost) ModuleInterface() string { return discoverModuleNetworkInterface() }

func (darwinHost) NetworkInterfaces() []macNetInterface { return discoverMacNetworkInterfaces() }

func (darwinHost) DefaultRoute() macDefaultRoute { return discoverMacDefaultRoute() }

func (darwinHost) InterfaceCounters() (map[string]networkByteCounters, error) {
	return discoverMacInterfaceCounters()
}

func (darwinHost) ProcessFlows(protocol string) ([]service.ActivityRecord, error) {
	out, err := exec.Command("nettop", "-L", "1", "-x", "-m", protocol, "-t", "wired").Output()
	if err != nil {
		return nil, err
	}
	return parseNettopActivity(string(out)), nil
}

var _ HostProbe = darwinHost{}
