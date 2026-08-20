package main

import (
	"strings"
	"time"
)

// ATTransport is the core's only view of the module. Everything above it —
// status polling, SMS, eSIM — speaks AT commands and nothing else, so this is
// the whole of what a platform has to supply. macOS drives it over libusb
// through cgo; Android will drive it from Kotlin's bulkTransfer over a gomobile
// callback. Neither is visible from here.
//
// openDJIUSBAT returns this interface rather than a concrete pointer on
// purpose: a nil *usbAT stored in an interface field is not a nil interface,
// and the code below tests a.usbAT != nil in a dozen places.
type ATTransport interface {
	// Command sends one AT command and returns the module's full response,
	// including its final OK or ERROR line.
	Command(cmd string, timeout time.Duration) (string, error)

	// CommandWithPrompt sends a command that answers with a "> " prompt, then
	// writes followUp once the prompt arrives. SMS submission needs it.
	CommandWithPrompt(cmd string, followUp []byte, timeout time.Duration) (string, error)

	// Close releases the underlying device. It must tolerate a module that has
	// already been unplugged.
	Close()

	// Description names the transport for logs and for the UI's port field.
	Description() string
}

// The rest of this file parses AT responses. It is pure text handling with no
// device access, which is why it belongs on this side of the seam rather than
// in the libusb implementation that happened to be its first caller.

func atResponseComplete(resp string) bool {
	normalized := strings.ReplaceAll(resp, "\r\n", "\n")
	return strings.Contains(normalized, "\nOK\n") ||
		strings.HasSuffix(normalized, "\nOK") ||
		atResponseIsError(normalized)
}

func atResponseIsError(resp string) bool {
	normalized := strings.ToUpper(strings.ReplaceAll(resp, "\r\n", "\n"))
	return strings.Contains(normalized, "\nERROR\n") ||
		strings.HasSuffix(normalized, "\nERROR") ||
		strings.Contains(normalized, "+CME ERROR:") ||
		strings.Contains(normalized, "+CMS ERROR:")
}

func atResponseHasPrompt(resp string) bool {
	trimmed := strings.TrimRight(resp, " \t\r\n")
	return strings.HasSuffix(trimmed, ">")
}

// A probe must receive OK. ERROR merely proves that a bulk interface accepted
// bytes; it is not the modem's AT channel (the QMI interface can do that).
func atProbeSucceeded(resp string) bool {
	normalized := strings.ReplaceAll(strings.TrimSpace(resp), "\r\n", "\n")
	return normalized == "OK" || strings.HasSuffix(normalized, "\nOK")
}

func normalizeATResponse(resp string) string {
	resp = strings.ReplaceAll(resp, "\r\r\n", "\r\n")
	resp = strings.TrimSpace(resp)
	lines := strings.Split(resp, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\r\n")
}
