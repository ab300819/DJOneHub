package core

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/ab300819/DJOneHub/internal/esim"
	"github.com/ab300819/DJOneHub/internal/modem"
	"github.com/ab300819/DJOneHub/internal/service"
	"github.com/ab300819/DJOneHub/pkg/smscodec"
	"github.com/damonto/euicc-go/driver"
)

type receivedSMS = service.ReceivedSMS

type profileNote = service.ProfileNote

type phonebookProbeResult = service.PhonebookProbe

type moduleProfileNote = service.ModuleProfileNote

type modulePhonebookEntry struct {
	Index  int
	Number string
	Text   string
}

type App struct {
	modem             *modem.Manager
	esimMu            sync.RWMutex
	esim              *esim.Manager
	esimSwitchAllowed bool
	usbAT             ATTransport
	openATTransport   func() (ATTransport, error)
	host              HostProbe
	port              string
	demo              bool
	discoveryError    string
	usbDevice         *usbDeviceStatus
	usbATBackoffUntil time.Time
	usbATBackoffErr   string

	smsMu          sync.RWMutex
	sms            []receivedSMS
	smsSendMu      sync.Mutex
	smsReassembler *smscodec.Reassembler

	smsPollInterval  time.Duration
	smsAutoCleanupME bool
	smsLastPoll      time.Time
	smsLastPollError string

	profileNotesMu     sync.Mutex
	profileNotes       map[string]profileNote
	profileNotesLoaded bool
	profileNotesPath   string

	moduleNotesMu sync.Mutex

	eventMu   sync.RWMutex
	eventSink EventSink

	trafficMu        sync.Mutex
	trafficBaselines map[string]NetworkByteCounters
}

// These live in internal/service so every transport shares one definition;
// the aliases keep the existing call sites in this file unchanged.
type usbDeviceStatus = service.USBDevice

type pdpContext = service.PDPContext

type macNetInterface = service.MacNetInterface

type macDefaultRoute = service.MacDefaultRoute

type NetworkByteCounters struct {
	RX uint64
	TX uint64
}

func (a *App) InitUSBATESIMManager() {
	if manager, _ := a.currentESIMManager(); manager != nil {
		return
	}
	esimManager, err := esim.NewManager(esim.ManagerOptions{
		DeviceID:  "mac-usbat",
		Transport: "custom",
		SmartCardChannelFactory: func() (driver.SmartCardChannel, error) {
			return newUSBATESIMChannel(a.runATCommand), nil
		},
	})
	if err != nil {
		log.Printf("eSIM manager unavailable over USB AT: %v", err)
		return
	}
	if a.InstallESIMManager(esimManager, true) {
		log.Printf("eSIM manager is available over USB AT with profile switching enabled")
	}
}

func (a *App) currentESIMManager() (*esim.Manager, bool) {
	a.esimMu.RLock()
	defer a.esimMu.RUnlock()
	return a.esim, a.esimSwitchAllowed
}

func (a *App) InstallESIMManager(manager *esim.Manager, switchAllowed bool) bool {
	if manager == nil {
		return false
	}
	a.esimMu.Lock()
	defer a.esimMu.Unlock()
	if a.esim != nil {
		return false
	}
	a.esim = manager
	a.esimSwitchAllowed = switchAllowed
	return true
}

// Options is what a host supplies to build a core. Everything in it is
// optional: a core with no transport and no probe reports an absent module
// rather than failing, which is the state the UI shows before anything is
// plugged in.
type Options struct {
	// Modem drives the module over a serial AT port, when the host found one.
	Modem *modem.Manager
	// Host answers questions about the machine. Supply one; the fallback
	// reports an absent machine.
	Host HostProbe
	// ATTransport drives the module over USB, used when there is no serial port.
	ATTransport ATTransport
	// OpenATTransport reopens the USB transport. A module can be unplugged and
	// plugged back in, and reopening it is a platform capability, so the core
	// is handed the means rather than knowing how. Without it a lost transport
	// stays lost until the process restarts.
	OpenATTransport func() (ATTransport, error)
	// Port names the channel in use, for the UI and the logs.
	Port string
	// DiscoveryError explains why no module was found, when none was.
	DiscoveryError string
	// USBDevice is the module as the host's USB inventory sees it.
	USBDevice *usbDeviceStatus
}

// New builds a core. The SMS polling defaults live here rather than at the call
// sites that used to repeat them.
func New(opts Options) *App {
	return &App{
		modem:            opts.Modem,
		host:             opts.Host,
		usbAT:            opts.ATTransport,
		openATTransport:  opts.OpenATTransport,
		port:             opts.Port,
		discoveryError:   opts.DiscoveryError,
		usbDevice:        opts.USBDevice,
		smsPollInterval:  8 * time.Second,
		smsAutoCleanupME: true,
		smsReassembler:   smscodec.NewReassembler(),
	}
}

// SetPort records the channel the module was reached on, once a transport that
// opened lazily can name it.
func (a *App) SetPort(port string) {
	a.port = port
	a.discoveryError = ""
}

func NewDemo(host HostProbe) *App {
	now := time.Now()
	return &App{
		demo:            true,
		host:            host,
		port:            "Demo · Quectel EG25-G",
		smsPollInterval: 8 * time.Second,
		sms: []receivedSMS{
			{
				Sender:    "10086",
				Content:   "【DJOneHub 演示】本月套餐剩余流量 18.6GB。",
				Timestamp: now.Add(-18 * time.Minute),
			},
			{
				Sender:    "+44 7400 123456",
				Content:   "Your verification code is 482913. It expires in 10 minutes.",
				Code:      "482913",
				Timestamp: now.Add(-2 * time.Hour),
			},
		},
	}
}

func (a *App) ensureUSBAT() error {
	if a.demo || a.modem != nil || a.usbAT != nil {
		return nil
	}
	if a.currentUSBDevice() == nil {
		a.port = "未检测到 DJI USB 设备"
		a.discoveryError = "DJI USB device is not connected"
		return errors.New("DJI USB device is not connected")
	}
	if !a.usbATBackoffUntil.IsZero() && time.Now().Before(a.usbATBackoffUntil) {
		if a.usbATBackoffErr != "" {
			return fmt.Errorf("USB AT is cooling down after disconnect: %s", a.usbATBackoffErr)
		}
		return errors.New("USB AT is cooling down after disconnect")
	}
	if a.openATTransport == nil {
		return errors.New("this build cannot open a USB AT transport")
	}
	dev, err := a.openATTransport()
	if err != nil {
		return err
	}
	a.usbAT = dev
	a.usbATBackoffUntil = time.Time{}
	a.usbATBackoffErr = ""
	a.port = dev.Description()
	a.discoveryError = ""
	log.Printf("USB AT bridge opened on DJI %s", dev.Description())
	// The first open may fail while USB is re-enumerating. When a later poll
	// succeeds, rebuild the eSIM service that startup could not create.
	a.InitUSBATESIMManager()
	return nil
}

func (a *App) resetUSBATIfGone(err error) {
	if err == nil || a.usbAT == nil {
		return
	}
	text := strings.ToUpper(err.Error())
	if !strings.Contains(text, "NO_DEVICE") &&
		!strings.Contains(text, "NOT_FOUND") &&
		!strings.Contains(text, "USB AT COMMAND TIMED OUT") {
		return
	}
	a.markUSBATDetached(err.Error())
}

// markUSBATDetached clears state belonging to a physically removed module.
// A later status/SMS poll will discover and open a newly connected module.
func (a *App) markUSBATDetached(reason string) {
	if a.usbAT != nil {
		log.Printf("USB AT bridge detached; waiting for a new enumeration: %s", reason)
		a.usbAT.Close()
		a.usbAT = nil
	}
	a.usbDevice = nil
	a.port = "未检测到 DJI USB 设备"
	a.discoveryError = "DJI USB device is not connected"
	a.usbATBackoffUntil = time.Now().Add(2 * time.Second)
	a.usbATBackoffErr = reason
	if manager, _ := a.currentESIMManager(); manager != nil {
		manager.NotifyModemReset()
	}
}

func (a *App) currentUSBDevice() *usbDeviceStatus {
	if a.modem != nil || a.demo {
		return a.usbDevice
	}
	usbDevice := a.probe().USBDevice()
	// Never retain the last successful scan: that is stale after an unplug.
	a.usbDevice = usbDevice
	return usbDevice
}
