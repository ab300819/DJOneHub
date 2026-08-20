//go:build !darwin || !cgo

package main

import (
	"errors"
	"time"
)

// This build has no way to reach the module: the libusb transport needs cgo and
// macOS. The stub exists so the platform-independent half of the program still
// compiles and can be type-checked here, which is the point of ATTransport.

type usbAT struct{}

func openDJIUSBAT() (ATTransport, error) {
	return nil, errors.New("USB AT requires macOS cgo build with libusb")
}

func (u *usbAT) Close() {}

func (u *usbAT) Command(_ string, _ time.Duration) (string, error) {
	return "", errors.New("USB AT is unavailable in this build")
}

func (u *usbAT) CommandWithPrompt(_ string, _ []byte, _ time.Duration) (string, error) {
	return "", errors.New("USB AT is unavailable in this build")
}

func (u *usbAT) Description() string {
	return "unavailable"
}

// Compile-time proof that both implementations still match the interface.
var _ ATTransport = (*usbAT)(nil)
