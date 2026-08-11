// Package service is the transport-independent surface of DJOneHub.
//
// The HTTP handlers, the stdio bridge used by the native app, and a future
// c-archive build for iPadOS all drive the module through this one interface.
// It owns no modem logic of its own: implementations delegate to the existing
// modem, eSIM and SMS packages, so adding a transport never means duplicating
// protocol code.
package service

import (
	"errors"

	"github.com/ab300819/DJOneHub/internal/modem"
)

// ErrInvalidCommand is returned when a caller passes something that is not an
// AT command. Transports map it to their own notion of a client-side error.
var ErrInvalidCommand = errors.New("command must start with AT")

// Service is the set of capabilities a transport may expose.
//
// It is deliberately incomplete: methods are added as transports come to need
// them, and handlers that have not been migrated keep working untouched.
type Service interface {
	// Health reports whether the core is up and what it is attached to.
	Health() Health

	// Status returns the current modem state. Both fields being nil is not
	// possible; see Status for which one is populated when.
	Status() (Status, error)

	// ExecuteAT sends a raw AT command and returns the module's reply verbatim.
	ExecuteAT(command string) (string, error)
}

// Health mirrors the payload of GET /api/health.
type Health struct {
	OK             bool       `json:"ok"`
	Port           string     `json:"port"`
	ESIMAvailable  bool       `json:"esim_available"`
	Demo           bool       `json:"demo"`
	USBDevice      *USBDevice `json:"usb_device"`
	DiscoveryError string     `json:"discovery_error"`
}

// Status carries whichever of the two shapes GET /api/status can produce.
//
// The distinction is real rather than incidental: with no AT channel the core
// can still see the USB device, and callers are told that instead of being
// given a status full of zero values.
type Status struct {
	// Device is set when an AT channel answered.
	Device *modem.DeviceStatus
	// Degraded is set when no AT channel is available.
	Degraded *DegradedStatus
}

// DegradedStatus describes what is known when no AT channel answered.
type DegradedStatus struct {
	Operator       string     `json:"operator"`
	SignalDBM      *int       `json:"signal_dbm"`
	NetworkMode    string     `json:"network_mode"`
	SimInserted    bool       `json:"sim_inserted"`
	HardwareStatus string     `json:"hardware_status"`
	DiscoveryError string     `json:"discovery_error"`
	USBDevice      *USBDevice `json:"usb_device"`
}

// USBDevice is the macOS USB inventory entry for the module.
type USBDevice struct {
	Product    string         `json:"product"`
	Vendor     string         `json:"vendor"`
	VendorID   string         `json:"vendor_id"`
	ProductID  string         `json:"product_id"`
	LocationID string         `json:"location_id"`
	Speed      string         `json:"speed"`
	Mode       string         `json:"mode"`
	Interfaces []USBInterface `json:"interfaces"`
}

// USBInterface is one interface of the module's current USB composition.
type USBInterface struct {
	Number    int `json:"number"`
	Class     int `json:"class"`
	Subclass  int `json:"subclass"`
	Protocol  int `json:"protocol"`
	Endpoints int `json:"endpoints"`
}
