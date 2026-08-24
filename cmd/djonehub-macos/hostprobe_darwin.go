//go:build darwin

package main

import (
	"os/exec"

	"github.com/ab300819/DJOneHub/core"
	"github.com/ab300819/DJOneHub/internal/service"
)

// darwinHost answers core.HostProbe by running the tools macOS ships with. The
// discovery and parsing functions it delegates to still live in main.go; the
// package move puts them here.
type darwinHost struct{}

func defaultHostProbe() core.HostProbe { return darwinHost{} }

func (darwinHost) USBDevice() *service.USBDevice { return discoverDJIUSBDevice() }

func (darwinHost) ATPort() (string, error) { return discoverATPort() }

func (darwinHost) ModuleInterface() string { return discoverModuleNetworkInterface() }

func (darwinHost) NetworkInterfaces() []service.MacNetInterface {
	return discoverMacNetworkInterfaces()
}

func (darwinHost) DefaultRoute() service.MacDefaultRoute { return discoverMacDefaultRoute() }

func (darwinHost) InterfaceCounters() (map[string]core.NetworkByteCounters, error) {
	return discoverMacInterfaceCounters()
}

func (darwinHost) ProcessFlows(protocol string) ([]service.ActivityRecord, error) {
	// -n keeps nettop from resolving names. Without it a single call takes some
	// thirty seconds, which is longer than the poll interval of every caller.
	// It costs nothing: measured against real output, the remote ends arrive as
	// addresses with or without resolution.
	out, err := exec.Command("nettop", "-n", "-L", "1", "-x", "-m", protocol, "-t", "wired").Output()
	if err != nil {
		return nil, err
	}
	return parseNettopActivity(string(out)), nil
}

var _ core.HostProbe = darwinHost{}
