// Package service is the transport-independent surface of DJOneHub.
//
// The HTTP handlers, the stdio bridge used by the native app, and a future
// c-archive build for iPadOS all drive the module through this one interface.
// It owns no modem logic of its own: implementations delegate to the existing
// modem, eSIM and SMS packages, so adding a transport never means duplicating
// protocol code.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ab300819/DJOneHub/internal/esim"
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

	// NetworkDiagnostic gathers the module and host state behind a data session.
	NetworkDiagnostic() (NetworkDiagnostic, error)
	// NetworkTraffic samples the module network interface's byte counters.
	NetworkTraffic() TrafficSnapshot
	// LocalNetworkConnection reports the module interface the host has up, or nil.
	LocalNetworkConnection() *LocalConnection
	// NetworkActivity samples the live connections riding the module's path.
	NetworkActivity() ActivitySnapshot
	// Check4GRoute reports whether the host currently routes through the module.
	Check4GRoute() NetworkCheckResult
	// CheckProxyRoute reports whether the configured local proxy reaches the internet.
	CheckProxyRoute() NetworkCheckResult
	// SetUSBNetMode changes the module's USB composition; it takes effect after a reboot.
	SetUSBNetMode(mode int) (USBNetResult, error)
	// RebootModule asks the module to restart.
	RebootModule() (RebootResult, error)

	// ESIMOverview reads the card's chip info and installed profiles.
	ESIMOverview() (ESIMOverviewResult, error)
	// ESIMHealth cross-checks the enabled profile against the module's registration.
	ESIMHealth() (ESIMHealthResult, error)
	// ListESIMNotes returns the locally stored per-ICCID notes.
	ListESIMNotes() (map[string]ProfileNote, error)
	// SaveESIMNote stores or clears a local note; empty fields delete it.
	SaveESIMNote(note ProfileNoteInput) (SavedNote, error)
	// ListModuleESIMNotes reads the notes kept in the module's own phonebook.
	ListModuleESIMNotes() (ModuleNotes, error)
	// SaveModuleESIMNote writes or deletes a note in the module's phonebook.
	SaveModuleESIMNote(note ModuleProfileNote) (ModuleNoteResult, error)
	// DownloadESIMProfile fetches a profile from an SM-DP+ server.
	DownloadESIMProfile(ctx context.Context, request ESIMDownloadRequest) (ESIMDownloadResult, error)
	// SwitchESIMProfile enables a different profile and restarts the module.
	SwitchESIMProfile(ctx context.Context, iccid, aid string) (ESIMSwitchResult, error)
	// DeleteESIMProfile removes a profile from the card.
	DeleteESIMProfile(iccid, aid string) (ESIMDeleteResult, error)
	// ProbeESIMPhonebook reports which phonebook operations the module supports.
	ProbeESIMPhonebook() PhonebookProbe
}

// ESIMOverviewResult is the card contents, or a note that it is a plain SIM.
//
// Demo payloads are fixture data rather than a real read, so they are carried
// as-is instead of being forced through the typed field.
type ESIMOverviewResult struct {
	PhysicalSIM bool
	Message     string
	Overview    *esim.EsimOverview
	DemoPayload any
}

// ESIMHealthResult pairs the enabled profile with the module's own view of it.
type ESIMHealthResult struct {
	PhysicalSIM   bool
	OK            bool
	Message       string
	ActiveProfile *esim.ProfileItem
	ModuleICCID   string
	IMSI          string
	Operator      string
	Registration  string
	Registered    bool
	SignalDBM     int
	NetworkMode   string
}

// ProfileNote is a locally stored note about one profile.
type ProfileNote struct {
	Label string `json:"label"`
	Phone string `json:"phone"`
	Tags  string `json:"tags"`
}

// ProfileNoteInput is a note submitted for storage.
type ProfileNoteInput struct {
	ICCID string `json:"iccid"`
	Label string `json:"label"`
	Phone string `json:"phone"`
	Tags  string `json:"tags"`
}

// SavedNote confirms a stored note.
type SavedNote struct {
	Message string      `json:"message"`
	Note    ProfileNote `json:"note"`
}

// ModuleProfileNote is a note kept in the module's own phonebook.
type ModuleProfileNote struct {
	Index int    `json:"index"`
	ICCID string `json:"iccid"`
	Label string `json:"label"`
	Phone string `json:"phone"`
	Tags  string `json:"tags"`
}

// ModuleNotes is the module phonebook's contents and capacity.
type ModuleNotes struct {
	Notes map[string]ModuleProfileNote `json:"notes"`
	Used  int                          `json:"used"`
	Total int                          `json:"total"`
}

// ModuleNoteResult confirms a phonebook write; Index is absent for deletions.
type ModuleNoteResult struct {
	Message string `json:"message"`
	Index   *int   `json:"index,omitempty"`
}

// ESIMDownloadRequest carries the activation details of a profile.
type ESIMDownloadRequest struct {
	SMDP             string `json:"smdp"`
	MatchingID       string `json:"matching_id"`
	ConfirmationCode string `json:"confirmation_code"`
	AID              string `json:"aid"`
	IMEI             string `json:"imei"`
}

// ESIMDownloadResult reports a completed download.
type ESIMDownloadResult struct {
	Demo    bool
	Message string
	Result  *esim.DownloadProfileResult
}

// ESIMDeleteResult reports a completed deletion.
type ESIMDeleteResult struct {
	Demo    bool
	Message string
	Result  *esim.DeleteProfileResult
}

// ESIMSwitchResult reports a profile switch and the module restart that follows
// it, which is needed because the modem can otherwise keep the previous SIM
// session alive.
type ESIMSwitchResult struct {
	Demo                  bool
	SwitchAccepted        bool
	Phase                 string
	TargetICCID           string
	RecoveryPending       bool
	ModuleRebootRequested bool
	ModuleRebootResponse  string
	ModuleRebootWarning   string
	ReconnectWaitSeconds  int
}

// PhonebookProbe reports which phonebook operations the module supports.
type PhonebookProbe struct {
	StorageSupported bool              `json:"storage_supported"`
	StorageSelected  bool              `json:"storage_selected"`
	ReadSupported    bool              `json:"read_supported"`
	WriteSupported   bool              `json:"write_supported"`
	StorageStatus    string            `json:"storage_status"`
	Responses        map[string]string `json:"responses"`
}

// NetworkDiagnostic is everything the core can say about data connectivity.
type NetworkDiagnostic struct {
	USBNetMode        string            `json:"usbnet_mode"`
	USBCfg            string            `json:"usbcfg"`
	PDPContexts       []PDPContext      `json:"pdp_contexts"`
	ActiveContexts    []int             `json:"active_contexts"`
	PDPAddresses      []string          `json:"pdp_addresses"`
	MacInterfaces     []MacNetInterface `json:"mac_interfaces"`
	DefaultRoute      MacDefaultRoute   `json:"default_route"`
	USBNetworkPresent bool              `json:"usb_network_present"`
	USBDevice         *USBDevice        `json:"usb_device,omitempty"`
	Raw               map[string]string `json:"raw,omitempty"`
	Errors            map[string]string `json:"errors,omitempty"`
}

// PDPContext is one packet data protocol context defined on the module.
type PDPContext struct {
	ID  int    `json:"id"`
	PDN string `json:"pdn"`
	APN string `json:"apn"`
}

// MacNetInterface is one host network interface.
type MacNetInterface struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	IPv4   string `json:"ipv4"`
	Kind   string `json:"kind"`
}

// MacDefaultRoute is the host's current default route.
type MacDefaultRoute struct {
	Interface string `json:"interface"`
	Gateway   string `json:"gateway"`
}

// NetworkCheckResult is the verdict of a connectivity probe.
type NetworkCheckResult struct {
	OK      bool   `json:"ok"`
	Summary string `json:"summary"`
	Detail  string `json:"detail"`
}

// TrafficSnapshot is one sample of the module interface's counters.
type TrafficSnapshot struct {
	Available    bool   `json:"available"`
	Interface    string `json:"interface,omitempty"`
	RXBytes      uint64 `json:"rx_bytes"`
	TXBytes      uint64 `json:"tx_bytes"`
	SessionRX    uint64 `json:"session_rx_bytes"`
	SessionTX    uint64 `json:"session_tx_bytes"`
	SessionTotal uint64 `json:"session_total_bytes"`
	SampledAtMS  int64  `json:"sampled_at_ms"`
	Error        string `json:"error,omitempty"`
}

// LocalConnection is the module network interface the host currently has up.
type LocalConnection struct {
	Interface string `json:"interface"`
	IPv4      string `json:"ipv4"`
	IsDefault bool   `json:"is_default"`
}

// ActivitySnapshot is one sample of the connections riding the module's path.
//
// Connections is capped and sorted by byte volume: the host sampler reports
// every process on the interface, which is more than the module's own traffic.
type ActivitySnapshot struct {
	Available         bool             `json:"available"`
	PhysicalInterface string           `json:"physical_interface,omitempty"`
	PhysicalIPv4      string           `json:"physical_ipv4,omitempty"`
	TunnelInterface   string           `json:"tunnel_interface,omitempty"`
	PhysicalActive    bool             `json:"physical_active"`
	SampledAtMS       int64            `json:"sampled_at_ms"`
	Connections       []ActivityRecord `json:"connections"`
}

// ActivityRecord is one process's flow to a remote host. Host and IP are
// exclusive: whichever form the sampler reported is the one that is set.
type ActivityRecord struct {
	Process   string `json:"process"`
	Host      string `json:"host,omitempty"`
	IP        string `json:"ip"`
	Port      string `json:"port,omitempty"`
	Protocol  string `json:"protocol"`
	Interface string `json:"interface"`
	State     string `json:"state,omitempty"`
	RXBytes   uint64 `json:"rx_bytes"`
	TXBytes   uint64 `json:"tx_bytes"`
}

// USBNetResult reports an accepted USB composition change.
type USBNetResult struct {
	Mode        int    `json:"mode"`
	Response    string `json:"response"`
	NeedsReboot bool   `json:"needs_reboot"`
}

// RebootResult reports an accepted module restart.
type RebootResult struct {
	Accepted bool   `json:"accepted"`
	Response string `json:"response"`
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
