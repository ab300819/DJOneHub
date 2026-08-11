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
	"fmt"
	"time"

	"github.com/ab300819/DJOneHub/internal/modem"
)

// ErrInvalidCommand is returned when a caller passes something that is not an
// AT command. Transports map it to their own notion of a client-side error.
var ErrInvalidCommand = Fail(KindInvalid, "command must start with AT")

// ErrorKind lets a transport report a failure in its own vocabulary without the
// service knowing anything about that transport. The HTTP handlers map these to
// status codes; the stdio bridge passes them through as a field.
type ErrorKind string

const (
	// KindInvalid means the caller sent something wrong.
	KindInvalid ErrorKind = "invalid"
	// KindUnavailable means the capability is not usable right now.
	KindUnavailable ErrorKind = "unavailable"
	// KindUpstream means the module or a lower layer failed.
	KindUpstream ErrorKind = "upstream"
	// KindConflict means the request contradicts current state.
	KindConflict ErrorKind = "conflict"
	// KindInternal means the core itself failed.
	KindInternal ErrorKind = "internal"
)

// Error is a failure carrying the kind a transport needs to classify it.
type Error struct {
	Kind    ErrorKind
	Message string
}

func (e *Error) Error() string { return e.Message }

// Fail builds a classified error.
func Fail(kind ErrorKind, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// KindOf reports how to classify err. Anything unclassified is treated as an
// upstream failure, which is what the majority of handlers already did.
func KindOf(err error) ErrorKind {
	var classified *Error
	if errors.As(err, &classified) {
		return classified.Kind
	}
	return KindUpstream
}

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

	// ListSMS returns the messages read so far this session.
	ListSMS() []ReceivedSMS
	// SMSStatus reports the polling state behind ListSMS.
	SMSStatus() SMSStatus
	// RefreshSMS asks the module for new messages.
	RefreshSMS() (RefreshResult, error)
	// ClearModuleSMS erases the module's own ME message store.
	ClearModuleSMS() (ClearResult, error)
	// SendSMS sends a text message and reports how many segments it took.
	SendSMS(phone, message string) (SendResult, error)
}

// ReceivedSMS is one message read from the module.
type ReceivedSMS struct {
	Sender    string    `json:"sender"`
	Content   string    `json:"content"`
	Code      string    `json:"code,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// SMSStatus describes the background polling that feeds ListSMS.
type SMSStatus struct {
	Count         int       `json:"count"`
	Polling       bool      `json:"polling"`
	PollIntervalS int       `json:"poll_interval_s"`
	AutoCleanupME bool      `json:"auto_cleanup_me"`
	LastPoll      time.Time `json:"last_poll"`
	LastPollError string    `json:"last_poll_error"`
}

// RefreshResult reports that a refresh was accepted; Count is present only when
// the refresh completed synchronously.
type RefreshResult struct {
	Accepted bool `json:"accepted"`
	Count    *int `json:"count,omitempty"`
}

// ClearResult reports the module store sizes around a cleanup.
type ClearResult struct {
	Cleared bool   `json:"cleared"`
	Memory  string `json:"memory,omitempty"`
	Before  int    `json:"before"`
	After   int    `json:"after"`
}

// SendResult reports a successful send.
type SendResult struct {
	Sent     bool `json:"sent"`
	Segments int  `json:"segments"`
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
