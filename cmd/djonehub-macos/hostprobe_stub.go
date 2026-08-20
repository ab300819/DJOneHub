//go:build !darwin

package main

// Nothing here can reach the machine, so the core gets the empty probe.
func defaultHostProbe() HostProbe { return unsupportedHost{} }
