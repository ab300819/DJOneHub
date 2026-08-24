//go:build !darwin

package main

import "github.com/ab300819/DJOneHub/core"

// Nothing here can reach the machine, so the core gets the empty probe.
func defaultHostProbe() core.HostProbe { return core.UnsupportedHost{} }
