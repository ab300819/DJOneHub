package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ab300819/DJOneHub/internal/backend"
	"github.com/ab300819/DJOneHub/internal/config"
	"github.com/ab300819/DJOneHub/internal/esim"
	"github.com/ab300819/DJOneHub/internal/modem"
	"github.com/ab300819/DJOneHub/internal/service"
	"github.com/ab300819/DJOneHub/pkg/smscodec"
	"github.com/damonto/euicc-go/driver"
)

//go:embed web/*
var webAssets embed.FS

type receivedSMS = service.ReceivedSMS

type profileNote = service.ProfileNote

type phonebookProbeResult = service.PhonebookProbe

type moduleProfileNote = service.ModuleProfileNote

type modulePhonebookEntry struct {
	Index  int
	Number string
	Text   string
}

type app struct {
	modem             *modem.Manager
	esimMu            sync.RWMutex
	esim              *esim.Manager
	esimSwitchAllowed bool
	usbAT             ATTransport
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
	trafficBaselines map[string]networkByteCounters
}

type usbInterfaceStatus = service.USBInterface

// These live in internal/service so every transport shares one definition;
// the aliases keep the existing call sites in this file unchanged.
type usbDeviceStatus = service.USBDevice

type pdpContext = service.PDPContext

type macNetInterface = service.MacNetInterface

type macDefaultRoute = service.MacDefaultRoute

type networkByteCounters struct {
	RX uint64
	TX uint64
}

func main() {
	var port string
	var listen string
	var demo bool
	flag.StringVar(&port, "port", "", "AT serial port; auto-detected when omitted")
	flag.StringVar(&listen, "listen", "127.0.0.1:7575", "HTTP listen address")
	flag.BoolVar(&demo, "demo", false, "run the web UI with simulated modem data")
	flag.IntVar(&parentPID, "parent-pid", 0, "exit when this parent process goes away; used by the macOS app")
	flag.BoolVar(&stdioMode, "stdio", false, "serve line-delimited JSON on stdin/stdout instead of HTTP")
	flag.Parse()

	if demo {
		instance := newDemoApp()
		log.Printf("DJOneHub demo mode")
		serve(instance, listen)
		return
	}

	if strings.TrimSpace(port) == "" {
		var err error
		port, err = discoverATPort()
		if err != nil {
			usbDevice := discoverDJIUSBDevice()
			usbATDevice, usbATErr := openDJIUSBAT()
			instance := &app{
				port:             "未发现 AT 串口",
				discoveryError:   err.Error(),
				usbDevice:        usbDevice,
				usbAT:            usbATDevice,
				smsPollInterval:  8 * time.Second,
				smsAutoCleanupME: true,
				smsReassembler:   smscodec.NewReassembler(),
			}
			if usbDevice != nil {
				log.Printf("DJI USB device detected without AT serial port: %s %s (%s:%s)",
					usbDevice.Vendor, usbDevice.Product, usbDevice.VendorID, usbDevice.ProductID)
			}
			if usbATErr != nil {
				log.Printf("USB AT unavailable: %v", usbATErr)
			} else {
				instance.port = usbATDevice.Description()
				instance.discoveryError = ""
				defer usbATDevice.Close()
				log.Printf("USB AT bridge opened on DJI %s", usbATDevice.Description())
				instance.initUSBATESIMManager()
			}
			log.Printf("modem discovery skipped: %v", err)
			go instance.startSMSPoller(context.Background())
			serve(instance, listen)
			return
		}
	}

	cfg := config.DeviceConfig{
		ID:            "mac-modem",
		Name:          "DJI 4G Module",
		ATPort:        port,
		ManagePort:    port,
		DeviceBackend: backend.BackendAT,
		ESIMTransport: "at",
		BaudRate:      115200,
		DataBits:      8,
		StopBits:      1,
		Parity:        "none",
		SMSEnabled:    true,
	}
	manager, err := modem.New(cfg)
	if err != nil {
		log.Fatalf("create modem manager: %v", err)
	}

	instance := &app{modem: manager, port: port, smsPollInterval: 8 * time.Second, smsAutoCleanupME: true}
	manager.SetSMSCallback(instance.recordSMS)
	if err := manager.Start(); err != nil {
		log.Fatalf("open modem on %s: %v", port, err)
	}
	defer manager.Stop()

	if !manager.WaitReady(15 * time.Second) {
		log.Printf("modem initialization is still running; the web UI will remain available")
	}

	atBackend := backend.NewATBackend(manager)
	esimManager, err := esim.NewManager(esim.ManagerOptions{
		DeviceID:  "mac-modem",
		Transport: "at",
		Modem:     manager,
		Backend:   atBackend,
	})
	if err != nil {
		log.Printf("eSIM manager unavailable: %v", err)
	} else {
		instance.installESIMManager(esimManager, false)
	}

	go manager.CheckAllSMS()

	serve(instance, listen)
}

func (a *app) initUSBATESIMManager() {
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
	if a.installESIMManager(esimManager, true) {
		log.Printf("eSIM manager is available over USB AT with profile switching enabled")
	}
}

func (a *app) currentESIMManager() (*esim.Manager, bool) {
	a.esimMu.RLock()
	defer a.esimMu.RUnlock()
	return a.esim, a.esimSwitchAllowed
}

func (a *app) installESIMManager(manager *esim.Manager, switchAllowed bool) bool {
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

// parentPID is set by -parent-pid. When the macOS app spawns this process it
// passes its own pid so the core cannot outlive its parent and keep holding the
// USB interface, which signal handlers alone cannot guarantee: a crashed or
// force-quit parent never gets to send a signal.
var parentPID int

// stdioMode is set by -stdio. The native app uses it so the core opens no
// socket at all: the only channel is the pipe pair it inherits from its parent.
var stdioMode bool

// watchParent closes the returned channel once the process is reparented, which
// on Darwin happens as soon as the original parent exits.
func watchParent(pid int) <-chan struct{} {
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			if os.Getppid() != pid {
				return
			}
			time.Sleep(time.Second)
		}
	}()
	return gone
}

func serve(instance *app, listen string) {
	if stdioMode {
		serveStdio(instance)
		return
	}

	server := &http.Server{
		Addr:              listen,
		Handler:           instance.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if !instance.demo {
		log.Printf("DJOneHub is using %s", instance.port)
	}
	log.Printf("Open http://%s", listen)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.ListenAndServe()
	}()

	var parentGone <-chan struct{}
	if parentPID > 0 {
		parentGone = watchParent(parentPID)
	}

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTP server stopped unexpectedly: %v", err)
		}
	case <-parentGone:
		log.Printf("DJOneHub parent process exited, stopping")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown: %v", err)
		}
	case <-ctx.Done():
		log.Printf("DJOneHub is stopping")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown: %v", err)
		}
	}
}

func newDemoApp() *app {
	now := time.Now()
	return &app{
		demo:            true,
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

func discoverATPort() (string, error) {
	var ports []string
	for _, pattern := range []string{
		"/dev/cu.usbmodem*",
		"/dev/cu.usbserial*",
		"/dev/cu.wchusbserial*",
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return "", err
		}
		ports = append(ports, matches...)
	}

	sort.SliceStable(ports, func(i, j int) bool {
		return portScore(ports[i]) > portScore(ports[j])
	})
	var attempted []string
	for _, port := range ports {
		attempted = append(attempted, port)
		if _, err := modem.ProbeIMEICached(port, 2*time.Second); err == nil {
			return port, nil
		}
	}
	if len(attempted) == 0 {
		return "", errors.New("no Quectel/DJI USB serial ports found; pass -port /dev/cu.* explicitly")
	}
	return "", fmt.Errorf("no AT-capable port found among %s", strings.Join(attempted, ", "))
}

func portScore(port string) int {
	name := strings.ToLower(port)
	if strings.Contains(name, "quectel") || strings.Contains(name, "dji") {
		return 100
	}
	if strings.Contains(name, "usbmodem") {
		return 80
	}
	if strings.Contains(name, "usbserial") {
		return 60
	}
	return 0
}

func discoverDJIUSBDevice() *usbDeviceStatus {
	out, err := exec.Command("ioreg", "-r", "-c", "IOUSBHostInterface", "-l", "-w", "0").Output()
	if err != nil {
		return nil
	}

	var device *usbDeviceStatus
	for _, block := range strings.Split(string(out), "\n\n") {
		vendorID, okVendor := intProperty(block, "idVendor")
		productID, okProduct := intProperty(block, "idProduct")
		if !okVendor || !okProduct || (vendorID != 0x2ca3 && vendorID != 0x2c7c) {
			continue
		}
		if device == nil {
			device = &usbDeviceStatus{
				Product:    stringProperty(block, "USB Product Name"),
				Vendor:     stringProperty(block, "USB Vendor Name"),
				VendorID:   fmt.Sprintf("%04x", vendorID),
				ProductID:  fmt.Sprintf("%04x", productID),
				LocationID: formatHexProperty(block, "locationID"),
				Speed:      usbSpeedName(intPropertyOrZero(block, "USBSpeed")),
				Mode:       "vendor-specific USB mode",
			}
			if strings.TrimSpace(device.Product) == "" {
				device.Product = "DJI 4G Module"
			}
			if strings.TrimSpace(device.Vendor) == "" {
				device.Vendor = "DJI"
			}
		}
		ifaceNumber, okIface := intProperty(block, "bInterfaceNumber")
		if !okIface {
			continue
		}
		iface := usbInterfaceStatus{
			Number:    ifaceNumber,
			Class:     intPropertyOrZero(block, "bInterfaceClass"),
			Subclass:  intPropertyOrZero(block, "bInterfaceSubClass"),
			Protocol:  intPropertyOrZero(block, "bInterfaceProtocol"),
			Endpoints: intPropertyOrZero(block, "bNumEndpoints"),
		}
		device.Interfaces = append(device.Interfaces, iface)
	}
	if device == nil {
		return nil
	}
	sort.SliceStable(device.Interfaces, func(i, j int) bool {
		return device.Interfaces[i].Number < device.Interfaces[j].Number
	})
	if allVendorSpecific(device.Interfaces) {
		device.Mode = "vendor-specific QMI/diagnostic mode"
	}
	return device
}

func allVendorSpecific(interfaces []usbInterfaceStatus) bool {
	if len(interfaces) == 0 {
		return false
	}
	for _, iface := range interfaces {
		if iface.Class != 255 {
			return false
		}
	}
	return true
}

func intPropertyOrZero(block, name string) int {
	value, _ := intProperty(block, name)
	return value
}

func intProperty(block, name string) (int, bool) {
	pattern := regexp.MustCompile(`"` + regexp.QuoteMeta(name) + `"\s*=\s*(\d+)`)
	match := pattern.FindStringSubmatch(block)
	if len(match) != 2 {
		return 0, false
	}
	value, err := strconv.Atoi(match[1])
	return value, err == nil
}

func stringProperty(block, name string) string {
	pattern := regexp.MustCompile(`"` + regexp.QuoteMeta(name) + `"\s*=\s*"([^"]*)"`)
	match := pattern.FindStringSubmatch(block)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func formatHexProperty(block, name string) string {
	value, ok := intProperty(block, name)
	if !ok {
		return ""
	}
	return fmt.Sprintf("0x%x", value)
}

func usbSpeedName(speed int) string {
	switch speed {
	case 1:
		return "low-speed"
	case 2:
		return "full-speed"
	case 3:
		return "high-speed"
	case 4:
		return "super-speed"
	default:
		if speed == 0 {
			return ""
		}
		return fmt.Sprintf("speed-%d", speed)
	}
}

func (a *app) recordSMS(sender, content string, timestamp time.Time) {
	a.mergeSMS([]receivedSMS{{
		Sender: sender, Content: content, Timestamp: timestamp,
	}})
}

func (a *app) mergeSMS(messages []receivedSMS) (newCount int, total int) {
	a.smsMu.Lock()
	defer a.smsMu.Unlock()
	seen := make(map[string]bool, len(a.sms)+len(messages))
	for _, item := range a.sms {
		seen[smsCacheKey(item)] = true
	}
	for _, item := range messages {
		if item.Code == "" {
			item.Code = extractSMSCode(item.Content)
		}
		key := smsCacheKey(item)
		if seen[key] {
			continue
		}
		seen[key] = true
		a.sms = append(a.sms, item)
		newCount++
	}
	sort.SliceStable(a.sms, func(i, j int) bool {
		return a.sms[i].Timestamp.After(a.sms[j].Timestamp)
	})
	if len(a.sms) > 500 {
		a.sms = a.sms[:500]
	}
	return newCount, len(a.sms)
}

func smsCacheKey(item receivedSMS) string {
	return item.Sender + "\x00" + item.Content + "\x00" + item.Timestamp.Format(time.RFC3339Nano)
}

func (a *app) setSMSPollStatus(err error) {
	a.smsMu.Lock()
	defer a.smsMu.Unlock()
	a.smsLastPoll = time.Now()
	if err != nil {
		a.smsLastPollError = err.Error()
		return
	}
	a.smsLastPollError = ""
}

func (a *app) startSMSPoller(ctx context.Context) {
	interval := a.smsPollInterval
	if interval <= 0 {
		interval = 8 * time.Second
	}
	timer := time.NewTimer(1200 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := a.pollSMSOnce(); err != nil {
				log.Printf("SMS poll failed: %v", err)
			}
			timer.Reset(interval)
		}
	}
}

func (a *app) pollSMSOnce() error {
	if a.demo || a.modem != nil {
		return nil
	}
	if err := a.ensureUSBAT(); err != nil {
		a.setSMSPollStatus(err)
		return err
	}
	messages, err := a.readUSBATSMS()
	if err != nil {
		a.resetUSBATIfGone(err)
		a.setSMSPollStatus(err)
		return err
	}
	newCount, total := a.mergeSMS(messages)
	if a.smsAutoCleanupME && len(messages) > 0 {
		before, after, cleanupErr := a.clearUSBATSMSMemory("ME")
		if cleanupErr != nil {
			log.Printf("auto cleanup ME SMS failed: %v", cleanupErr)
		} else if before != after {
			log.Printf("auto cleanup ME SMS: %d -> %d", before, after)
		}
	}
	a.setSMSPollStatus(nil)
	if newCount > 0 {
		log.Printf("SMS poll cached %d new message(s), total %d", newCount, total)
	}
	return nil
}

func (a *app) ensureUSBAT() error {
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
	dev, err := openDJIUSBAT()
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
	a.initUSBATESIMManager()
	return nil
}

func (a *app) resetUSBATIfGone(err error) {
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
func (a *app) markUSBATDetached(reason string) {
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

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("GET /api/status", a.status)
	mux.HandleFunc("GET /api/sms", a.listSMS)
	mux.HandleFunc("GET /api/sms/status", a.smsStatus)
	mux.HandleFunc("POST /api/sms/send", a.sendSMS)
	mux.HandleFunc("POST /api/sms/refresh", a.refreshSMS)
	mux.HandleFunc("POST /api/sms/clear-module", a.clearModuleSMS)
	mux.HandleFunc("POST /api/at", a.executeAT)
	mux.HandleFunc("GET /api/network", a.networkDiagnostic)
	mux.HandleFunc("GET /api/network/traffic", a.networkTraffic)
	mux.HandleFunc("GET /api/network/local", a.localNetworkConnection)
	mux.HandleFunc("GET /api/network/activity", a.networkActivity)
	mux.HandleFunc("POST /api/network/check-4g", a.check4GRoute)
	mux.HandleFunc("POST /api/network/check-proxy", a.checkProxyRoute)
	mux.HandleFunc("POST /api/network/usbnet", a.setUSBNetMode)
	mux.HandleFunc("POST /api/network/reboot-module", a.rebootModule)
	mux.HandleFunc("GET /api/esim", a.esimOverview)
	mux.HandleFunc("GET /api/esim/notes", a.listESIMNotes)
	mux.HandleFunc("PUT /api/esim/notes", a.saveESIMNote)
	mux.HandleFunc("GET /api/esim/module-notes", a.listModuleESIMNotes)
	mux.HandleFunc("PUT /api/esim/module-notes", a.saveModuleESIMNote)
	mux.HandleFunc("GET /api/esim/health", a.esimHealth)
	mux.HandleFunc("POST /api/esim/phonebook/probe", a.probeESIMPhonebook)
	mux.HandleFunc("POST /api/esim/switch", a.switchESIM)
	mux.HandleFunc("PATCH /api/esim/profile", a.renameESIMProfile)
	mux.HandleFunc("DELETE /api/esim/profile", a.deleteESIMProfile)
	mux.HandleFunc("POST /api/esim/download", a.downloadESIMProfile)
	content, _ := fs.Sub(webAssets, "web")
	mux.Handle("/", http.FileServer(http.FS(content)))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (a *app) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.Health())
}

func (a *app) status(w http.ResponseWriter, _ *http.Request) {
	status, err := a.Status()
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if status.Device != nil {
		writeJSON(w, http.StatusOK, status.Device)
		return
	}
	writeJSON(w, http.StatusOK, status.Degraded)
}

func (a *app) currentUSBDevice() *usbDeviceStatus {
	if a.modem != nil || a.demo {
		return a.usbDevice
	}
	usbDevice := a.probe().USBDevice()
	// Never retain the last successful scan: that is stale after an unplug.
	a.usbDevice = usbDevice
	return usbDevice
}

func (a *app) usbATStatus() (modem.DeviceStatus, error) {
	firmwareResp, _ := a.usbAT.Command("ATI", 3*time.Second)
	cpinResp, cpinErr := a.usbAT.Command("AT+CPIN?", 3*time.Second)
	csqResp, _ := a.usbAT.Command("AT+CSQ", 3*time.Second)
	ceregResp, _ := a.usbAT.Command("AT+CEREG?", 3*time.Second)
	cregResp, _ := a.usbAT.Command("AT+CREG?", 3*time.Second)
	_, _ = a.usbAT.Command("AT+COPS=3,2", 3*time.Second)
	copsResp, _ := a.usbAT.Command("AT+COPS?", 3*time.Second)
	qccidResp, _ := a.usbAT.Command("AT+QCCID", 3*time.Second)
	cimiResp, _ := a.usbAT.Command("AT+CIMI", 3*time.Second)
	qnwinfoResp, _ := a.usbAT.Command("AT+QNWINFO", 3*time.Second)
	usbnetResp, _ := a.usbAT.Command(`AT+QCFG="usbnet"`, 3*time.Second)

	if cpinErr != nil {
		return modem.DeviceStatus{}, cpinErr
	}

	regStatus := firstNonZeroRegistration(ceregResp, cregResp)
	mode, duplex, band, channel := parseUSBATQNWInfo(qnwinfoResp)
	usbnetMode := -1
	if parsedMode, err := strconv.Atoi(parseUSBNetMode(usbnetResp)); err == nil {
		usbnetMode = parsedMode
	}
	status := modem.DeviceStatus{
		Firmware:      parseUSBATFirmware(firmwareResp),
		ICCID:         parseUSBATPrefixed(qccidResp, "+QCCID:"),
		IMSI:          parseUSBATIMSI(cimiResp),
		Operator:      parseUSBATOperator(copsResp),
		SimInserted:   strings.Contains(strings.ToUpper(cpinResp), "READY"),
		SignalDBM:     parseUSBATCSQDBM(csqResp),
		RegStatus:     regStatus,
		RegStatusText: registrationText(regStatus),
		NetworkMode:   mode,
		NetworkDuplex: duplex,
		RadioBand:     band,
		RadioChannel:  channel,
		USBNetMode:    usbnetMode,
	}
	if status.Operator == "" && strings.Contains(copsResp, "CHN-UNICOM") {
		status.Operator = "CHN-UNICOM"
	}
	return status, nil
}

func parseUSBATFirmware(resp string) string {
	lines := splitATLines(resp)
	var useful []string
	for _, line := range lines {
		up := strings.ToUpper(line)
		if strings.HasPrefix(up, "ATI") || up == "OK" {
			continue
		}
		useful = append(useful, line)
	}
	return strings.Join(useful, " · ")
}

func splitATLines(resp string) []string {
	resp = strings.ReplaceAll(resp, "\r", "\n")
	raw := strings.Split(resp, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func parseUSBATPrefixed(resp, prefix string) string {
	for _, line := range splitATLines(resp) {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func parseUSBATIMSI(resp string) string {
	for _, line := range splitATLines(resp) {
		up := strings.ToUpper(line)
		if up == "OK" || strings.HasPrefix(up, "AT") {
			continue
		}
		if _, err := strconv.ParseUint(line, 10, 64); err == nil && len(line) >= 5 {
			return line
		}
	}
	return ""
}

func parseUSBATCSQDBM(resp string) int {
	re := regexp.MustCompile(`\+CSQ:\s*(\d+),`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 2 {
		return 0
	}
	rssi, err := strconv.Atoi(match[1])
	if err != nil || rssi == 99 {
		return 0
	}
	return -113 + 2*rssi
}

func parseUSBATOperator(resp string) string {
	re := regexp.MustCompile(`\+COPS:\s*\d+,\d+,"([^"]*)"`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 2 {
		return ""
	}
	return modem.ResolveServingOperatorNameFromPLMN(match[1])
}

func firstNonZeroRegistration(responses ...string) int {
	for _, resp := range responses {
		re := regexp.MustCompile(`\+(?:CE)?REG:\s*\d+,(\d+)`)
		match := re.FindStringSubmatch(resp)
		if len(match) != 2 {
			continue
		}
		status, err := strconv.Atoi(match[1])
		if err == nil && status != 0 {
			return status
		}
	}
	return 0
}

func registrationText(status int) string {
	switch status {
	case 1:
		return "已注册"
	case 5:
		return "漫游注册"
	case 2:
		return "搜索中"
	case 3:
		return "注册被拒绝"
	default:
		return "未注册"
	}
}

func parseUSBATQNWInfo(resp string) (mode, duplex, band string, channel uint32) {
	re := regexp.MustCompile(`\+QNWINFO:\s*"([^"]*)","[^"]*","([^"]*)",(\d+)`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 4 {
		return "", "", "", 0
	}
	mode = match[1]
	radio := match[2]
	if strings.Contains(strings.ToUpper(mode), "FDD") {
		duplex = "FDD"
		mode = strings.TrimSpace(strings.TrimPrefix(mode, "FDD"))
	}
	if strings.Contains(strings.ToUpper(mode), "TDD") {
		duplex = "TDD"
		mode = strings.TrimSpace(strings.TrimPrefix(mode, "TDD"))
	}
	band = strings.TrimPrefix(radio, "LTE ")
	if value, err := strconv.ParseUint(match[3], 10, 32); err == nil {
		channel = uint32(value)
	}
	return mode, duplex, band, channel
}

func (a *app) readUSBATSMS() ([]receivedSMS, error) {
	if _, err := a.usbAT.Command("AT+CMGF=0", 3*time.Second); err != nil {
		return nil, fmt.Errorf("set SMS PDU mode: %w", err)
	}
	memories := []string{"SM", "ME"}
	seen := make(map[string]bool)
	messages := make([]receivedSMS, 0)
	var errs []string
	for _, memory := range memories {
		items, err := a.readUSBATSMSFromMemory(memory)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", memory, err))
			continue
		}
		for _, item := range items {
			key := item.Sender + "\x00" + item.Content + "\x00" + item.Timestamp.Format(time.RFC3339Nano)
			if seen[key] {
				continue
			}
			seen[key] = true
			messages = append(messages, item)
		}
	}
	if len(messages) == 0 && len(errs) == len(memories) {
		return nil, fmt.Errorf("list SMS failed: %s", strings.Join(errs, "; "))
	}
	sort.SliceStable(messages, func(i, j int) bool {
		return messages[i].Timestamp.After(messages[j].Timestamp)
	})
	return messages, nil
}

func (a *app) readUSBATSMSFromMemory(memory string) ([]receivedSMS, error) {
	if _, err := a.usbAT.Command(fmt.Sprintf(`AT+CPMS="%s","%s","%s"`, memory, memory, memory), 5*time.Second); err != nil {
		return nil, fmt.Errorf("select storage: %w", err)
	}
	resp, err := a.usbAT.Command("AT+CMGL=4", 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("list SMS: %w", err)
	}
	pdus := parseUSBATCMGL(resp)
	messages := make([]receivedSMS, 0, len(pdus))
	for _, item := range pdus {
		msg, concat, err := decodeUSBATPDU(item.header, item.pdu)
		if err != nil {
			messages = append(messages, receivedSMS{
				Sender:    "PDU",
				Content:   fmt.Sprintf("[短信解析失败] %v\n%s", err, item.pdu),
				Timestamp: time.Now(),
			})
			continue
		}
		if concat.IsConcat {
			if a.smsReassembler == nil {
				a.smsReassembler = smscodec.NewReassembler()
			}
			complete, content := a.smsReassembler.Add(msg.Sender, concat, msg.Content)
			if !complete {
				continue
			}
			msg.Content = content
			log.Printf("USB AT long SMS reassembled: sender=%s segments=%d", msg.Sender, concat.Total)
		}
		messages = append(messages, msg)
	}
	if a.smsReassembler != nil {
		a.smsReassembler.Cleanup(10 * time.Minute)
	}
	sort.SliceStable(messages, func(i, j int) bool {
		return messages[i].Timestamp.After(messages[j].Timestamp)
	})
	return messages, nil
}

func (a *app) clearUSBATSMSMemory(memory string) (before, after int, err error) {
	resp, err := a.usbAT.Command(fmt.Sprintf(`AT+CPMS="%s","%s","%s"`, memory, memory, memory), 5*time.Second)
	if err != nil {
		return 0, 0, fmt.Errorf("select storage: %w", err)
	}
	before = parseUSBATCPMSUsed(resp)
	if _, err := a.usbAT.Command("AT+CMGD=1,4", 20*time.Second); err != nil {
		return before, 0, fmt.Errorf("delete messages: %w", err)
	}
	resp, err = a.usbAT.Command(fmt.Sprintf(`AT+CPMS="%s","%s","%s"`, memory, memory, memory), 5*time.Second)
	if err != nil {
		return before, 0, fmt.Errorf("recheck storage: %w", err)
	}
	after = parseUSBATCPMSUsed(resp)
	return before, after, nil
}

func parseUSBATCPMSUsed(resp string) int {
	re := regexp.MustCompile(`\+CPMS:\s*(\d+),`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 2 {
		return 0
	}
	used, err := strconv.Atoi(match[1])
	if err != nil {
		return 0
	}
	return used
}

type usbATSMSPDU struct {
	header string
	pdu    string
}

func parseUSBATCMGL(resp string) []usbATSMSPDU {
	lines := splitATLines(resp)
	var out []usbATSMSPDU
	for i := 0; i < len(lines)-1; i++ {
		if !strings.HasPrefix(lines[i], "+CMGL:") {
			continue
		}
		next := strings.TrimSpace(lines[i+1])
		if !smscodec.IsHexString(next) {
			continue
		}
		pdu, _ := smscodec.TrimFullPDUHexByATHeader(next, lines[i])
		out = append(out, usbATSMSPDU{header: lines[i], pdu: pdu})
		i++
	}
	return out
}

func decodeUSBATPDU(header, pduHex string) (receivedSMS, smscodec.ConcatInfo, error) {
	raw := strings.TrimSpace(pduHex)
	if trimmed, ok := smscodec.TrimFullPDUHexByATHeader(raw, header); ok {
		raw = trimmed
	}
	full, err := hex.DecodeString(raw)
	if err != nil {
		return receivedSMS{}, smscodec.ConcatInfo{}, err
	}
	if len(full) < 2 {
		return receivedSMS{}, smscodec.ConcatInfo{}, errors.New("PDU too short")
	}
	smscLen := int(full[0])
	tpduOffset := 1 + smscLen
	if tpduOffset >= len(full) {
		return receivedSMS{}, smscodec.ConcatInfo{}, errors.New("PDU has invalid SMSC length")
	}
	sender, content, timestamp, concat, err := smscodec.DecodeDeliverTPDU(full[tpduOffset:])
	if err != nil {
		return receivedSMS{}, smscodec.ConcatInfo{}, err
	}
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	return receivedSMS{Sender: sender, Content: content, Timestamp: timestamp}, concat, nil
}

func (a *app) listSMS(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.ListSMS())
}

func (a *app) smsStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.SMSStatus())
}

func (a *app) refreshSMS(w http.ResponseWriter, _ *http.Request) {
	result, err := a.RefreshSMS()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (a *app) clearModuleSMS(w http.ResponseWriter, _ *http.Request) {
	result, err := a.ClearModuleSMS()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) runATCommand(command string, timeout time.Duration) (string, error) {
	if a.demo {
		responses := map[string]string{
			"AT":                 "OK",
			"AT+CSQ":             "+CSQ: 22,99\r\nOK",
			"AT+COPS?":           "+COPS: 0,0,\"China Mobile\",7\r\nOK",
			"AT+QNWINFO":         "+QNWINFO: \"FDD LTE\",\"46000\",\"LTE BAND 3\",1650\r\nOK",
			"AT+QCFG=\"USBNET\"": "+QCFG: \"usbnet\",1\r\nOK",
			"AT+QCFG=\"USBCFG\"": "+QCFG: \"usbcfg\",0x2C7C,0x0125,1,1,1,1,1,0,0\r\nOK",
			"AT+CGDCONT?":        "+CGDCONT: 1,\"IPV4V6\",\"3gnet\",\"0.0.0.0\",0,0,0,0\r\nOK",
			"AT+CGACT?":          "+CGACT: 1,1\r\nOK",
			"AT+CGPADDR=1":       "+CGPADDR: 1,\"10.23.45.67\"\r\nOK",
		}
		response := responses[strings.ToUpper(strings.TrimSpace(command))]
		if response == "" {
			response = "OK"
		}
		return response, nil
	}
	if a.modem == nil {
		if err := a.ensureUSBAT(); err != nil {
			return "", err
		}
		if a.usbAT == nil {
			return "", errors.New("AT serial port is unavailable")
		}
		response, err := a.usbAT.Command(command, timeout)
		if err != nil {
			a.resetUSBATIfGone(err)
		}
		return response, err
	}
	return a.modem.ExecuteAT(command, timeout)
}

func (a *app) sendSMS(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone   string `json:"phone"`
		Message string `json:"message"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.SendSMS(body.Phone, body.Message)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) sendTextSMS(phone, message string) (int, error) {
	if a.modem == nil {
		return a.sendUSBATSMS(phone, message)
	}
	if err := a.modem.SendSMSWithOptions(phone, message, smsSubmitOptions(message)); err != nil {
		return 0, err
	}
	return 1, nil
}

func (a *app) sendUSBATSMS(phone, message string) (int, error) {
	a.smsSendMu.Lock()
	defer a.smsSendMu.Unlock()

	if err := a.ensureUSBAT(); err != nil {
		return 0, err
	}
	if a.usbAT == nil {
		return 0, errors.New("AT serial port is unavailable")
	}

	modeResponse, err := a.usbAT.Command("AT+CMGF=0", 5*time.Second)
	if err != nil {
		a.resetUSBATIfGone(err)
		return 0, fmt.Errorf("set SMS PDU mode: %w", err)
	}
	if !atProbeSucceeded(modeResponse) {
		return 0, fmt.Errorf("set SMS PDU mode failed: %s", modeResponse)
	}

	tpdus, tpduLengths, err := smscodec.BuildSubmitTPDUsWithOptions(phone, message, smsSubmitOptions(message))
	if err != nil {
		return 0, fmt.Errorf("build SMS PDU: %w", err)
	}
	for i, tpdu := range tpdus {
		pdu := append([]byte{0x00}, tpdu...)
		payload := []byte(strings.ToUpper(hex.EncodeToString(pdu)) + "\x1a")
		response, sendErr := a.usbAT.CommandWithPrompt(
			fmt.Sprintf("AT+CMGS=%d", tpduLengths[i]),
			payload,
			45*time.Second,
		)
		if sendErr != nil {
			a.resetUSBATIfGone(sendErr)
			return i, fmt.Errorf("send SMS segment %d/%d: %w", i+1, len(tpdus), sendErr)
		}
		if atResponseIsError(response) || !strings.Contains(response, "+CMGS:") || !atProbeSucceeded(response) {
			return i, fmt.Errorf("send SMS segment %d/%d failed: %s", i+1, len(tpdus), response)
		}
		if i+1 < len(tpdus) {
			time.Sleep(500 * time.Millisecond)
		}
	}
	return len(tpdus), nil
}

func smsSubmitOptions(message string) smscodec.SubmitOptions {
	for _, r := range message {
		if r > 127 {
			return smscodec.SubmitOptions{Encoding: smscodec.SMSEncodingUCS2}
		}
	}
	return smscodec.SubmitOptions{}
}

// writeServiceError maps a service failure onto the status code this API has
// always used for that kind of failure.
func writeServiceError(w http.ResponseWriter, err error) {
	switch service.KindOf(err) {
	case service.KindInvalid:
		writeError(w, http.StatusBadRequest, err.Error())
	case service.KindUnavailable:
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case service.KindConflict:
		writeError(w, http.StatusConflict, err.Error())
	case service.KindInternal:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeError(w, http.StatusBadGateway, err.Error())
	}
}

func (a *app) executeAT(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Command string `json:"command"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	response, err := a.ExecuteAT(body.Command)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"response": response})
}

func (a *app) networkDiagnostic(w http.ResponseWriter, _ *http.Request) {
	diag, err := a.NetworkDiagnostic()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, diag)
}

func (a *app) networkTraffic(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.NetworkTraffic())
}

func (a *app) localNetworkConnection(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.LocalNetworkConnection())
}

func (a *app) networkActivity(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.NetworkActivity())
}

func sessionTrafficFromCounters(current, baseline networkByteCounters) (rx, tx, total uint64) {
	rx = current.RX - baseline.RX
	tx = current.TX - baseline.TX
	return rx, tx, rx + tx
}

func (a *app) check4GRoute(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.Check4GRoute())
}

func (a *app) checkProxyRoute(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.CheckProxyRoute())
}

func (a *app) setUSBNetMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode int `json:"mode"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.SetUSBNetMode(body.Mode)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) rebootModule(w http.ResponseWriter, _ *http.Request) {
	result, err := a.RebootModule()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func parseUSBNetMode(resp string) string {
	re := regexp.MustCompile(`\+QCFG:\s*"usbnet",(\d+)`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func parsePDPContexts(resp string) []pdpContext {
	re := regexp.MustCompile(`\+CGDCONT:\s*(\d+),"([^"]*)","([^"]*)"`)
	var contexts []pdpContext
	for _, match := range re.FindAllStringSubmatch(resp, -1) {
		id, _ := strconv.Atoi(match[1])
		contexts = append(contexts, pdpContext{ID: id, PDN: match[2], APN: match[3]})
	}
	return contexts
}

func parseActivePDPContexts(resp string) []int {
	re := regexp.MustCompile(`\+CGACT:\s*(\d+),1`)
	var contexts []int
	for _, match := range re.FindAllStringSubmatch(resp, -1) {
		id, _ := strconv.Atoi(match[1])
		contexts = append(contexts, id)
	}
	return contexts
}

func parsePDPAddresses(resp string) []string {
	re := regexp.MustCompile(`\+CGPADDR:\s*\d+,"([^"]*)"`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 2 {
		return nil
	}
	var out []string
	for _, part := range strings.Split(match[1], ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func discoverMacNetworkInterfaces() []macNetInterface {
	out, err := exec.Command("ifconfig").Output()
	if err != nil {
		return nil
	}
	var interfaces []macNetInterface
	for _, block := range splitIfconfigBlocks(string(out)) {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		lines := strings.Split(block, "\n")
		if len(lines) == 0 {
			continue
		}
		fields := strings.Fields(lines[0])
		if len(fields) == 0 {
			continue
		}
		name := strings.TrimSuffix(fields[0], ":")
		if name == "" || strings.HasPrefix(name, "lo") || strings.HasPrefix(name, "utun") {
			continue
		}
		item := macNetInterface{Name: name, Status: "unknown", Kind: classifyMacInterfaceName(name)}
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "status:") {
				item.Status = strings.TrimSpace(strings.TrimPrefix(line, "status:"))
			}
			if strings.HasPrefix(line, "inet ") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					item.IPv4 = fields[1]
				}
			}
		}
		interfaces = append(interfaces, item)
	}
	return interfaces
}

func discoverMacDefaultRoute() macDefaultRoute {
	out, err := exec.Command("route", "-n", "get", "default").Output()
	if err != nil {
		return macDefaultRoute{}
	}
	var route macDefaultRoute
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "gateway:") {
			route.Gateway = strings.TrimSpace(strings.TrimPrefix(line, "gateway:"))
		}
		if strings.HasPrefix(line, "interface:") {
			route.Interface = strings.TrimSpace(strings.TrimPrefix(line, "interface:"))
		}
	}
	return route
}

func splitIfconfigBlocks(out string) []string {
	var blocks []string
	var current []string
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if line[0] != '\t' && line[0] != ' ' && strings.Contains(line, ":") {
			if len(current) > 0 {
				blocks = append(blocks, strings.Join(current, "\n"))
			}
			current = []string{line}
			continue
		}
		if len(current) > 0 {
			current = append(current, line)
		}
	}
	if len(current) > 0 {
		blocks = append(blocks, strings.Join(current, "\n"))
	}
	return blocks
}

func classifyMacInterfaceName(name string) string {
	switch {
	case strings.HasPrefix(name, "en"):
		return "ethernet"
	case strings.HasPrefix(name, "bridge"):
		return "bridge"
	case strings.HasPrefix(name, "awdl") || strings.HasPrefix(name, "llw") || strings.HasPrefix(name, "ap"):
		return "apple-wireless"
	default:
		return "other"
	}
}

func hasLikelyUSBNetworkInterface(interfaces []macNetInterface) bool {
	for _, item := range interfaces {
		if item.Kind == "ethernet" && item.Name != "en0" && item.Status == "active" {
			return true
		}
	}
	return false
}

func selectUSBTrafficInterface(interfaces []macNetInterface, route macDefaultRoute) string {
	for _, item := range interfaces {
		if item.Name == route.Interface && item.Kind == "ethernet" && item.Name != "en0" && item.Status == "active" {
			return item.Name
		}
	}
	for _, item := range interfaces {
		if item.Kind == "ethernet" && item.Name != "en0" && item.Status == "active" {
			return item.Name
		}
	}
	return ""
}

// nettopProcessSuffix strips the pid that nettop appends to a process name.
var nettopProcessSuffix = regexp.MustCompile(`\.\d+$`)

// parseNettopActivity reads `nettop -x` CSV. Rows without a flow name a process
// and apply to the flow rows that follow, so the current process is carried
// forward across iterations.
func parseNettopActivity(out string) []service.ActivityRecord {
	reader := csv.NewReader(strings.NewReader(out))
	reader.FieldsPerRecord = -1
	var records []service.ActivityRecord
	process := "系统"
	for {
		row, err := reader.Read()
		if err != nil {
			break
		}
		if len(row) < 6 || row[1] == "" {
			continue
		}
		description := strings.TrimSpace(row[1])
		iface := strings.TrimSpace(row[2])
		if !strings.Contains(description, "<->") {
			process = nettopProcessSuffix.ReplaceAllString(description, "")
			continue
		}
		protocol, host, port := parseNettopFlow(description)
		if host == "" || iface == "" || host == "127.0.0.1" || host == "::1" {
			continue
		}
		rx, _ := strconv.ParseUint(strings.TrimSpace(row[4]), 10, 64)
		tx, _ := strconv.ParseUint(strings.TrimSpace(row[5]), 10, 64)
		state := ""
		if strings.HasPrefix(protocol, "tcp") && len(row) > 3 {
			state = strings.TrimSpace(row[3])
		}
		record := service.ActivityRecord{
			Process: process, Port: port, Protocol: protocol,
			Interface: iface, State: state, RXBytes: rx, TXBytes: tx,
		}
		if net.ParseIP(host) == nil {
			record.Host = host
		} else {
			record.IP = host
		}
		records = append(records, record)
	}
	return records
}

// parseNettopFlow splits a "proto local<->remote" description, returning the
// remote end. IPv6 addresses carry colons of their own, so the port is split on
// the last dot for those and on the last colon otherwise.
func parseNettopFlow(description string) (protocol, host, port string) {
	fields := strings.Fields(description)
	if len(fields) < 2 {
		return "", "", ""
	}
	protocol = fields[0]
	flow := fields[len(fields)-1]
	parts := strings.SplitN(flow, "<->", 2)
	if len(parts) != 2 {
		return protocol, "", ""
	}
	remote := parts[1]
	if strings.Count(remote, ":") > 1 {
		if index := strings.LastIndex(remote, "."); index > 0 {
			return protocol, strings.Trim(remote[:index], "[]"), remote[index+1:]
		}
		return protocol, strings.Trim(remote, "[]"), ""
	}
	if index := strings.LastIndex(remote, ":"); index > 0 {
		return protocol, strings.Trim(remote[:index], "[]"), remote[index+1:]
	}
	return protocol, strings.Trim(remote, "[]"), ""
}

func discoverMacInterfaceCounters() (map[string]networkByteCounters, error) {
	out, err := exec.Command("netstat", "-ibn").Output()
	if err != nil {
		return nil, err
	}
	return parseMacInterfaceCounters(string(out)), nil
}

func parseMacInterfaceCounters(out string) map[string]networkByteCounters {
	counters := make(map[string]networkByteCounters)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || !strings.HasPrefix(fields[2], "<Link#") {
			continue
		}
		name := strings.TrimSuffix(fields[0], "*")
		rx, rxErr := strconv.ParseUint(fields[6], 10, 64)
		tx, txErr := strconv.ParseUint(fields[9], 10, 64)
		if name == "" || rxErr != nil || txErr != nil {
			continue
		}
		counters[name] = networkByteCounters{RX: rx, TX: tx}
	}
	return counters
}

func (a *app) loadProfileNotesLocked() error {
	if a.profileNotesLoaded {
		return nil
	}
	path := a.profileNotesPath
	if path == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("locate profile notes directory: %w", err)
		}
		path = filepath.Join(configDir, "DJOneHub", "profile-notes.json")
		a.profileNotesPath = path
	}
	notes := make(map[string]profileNote)
	readPath := path
	if _, err := os.Stat(readPath); errors.Is(err, os.ErrNotExist) {
		legacyPath := filepath.Join(filepath.Dir(filepath.Dir(path)), "VoHive macOS", "profile-notes.json")
		if _, legacyErr := os.Stat(legacyPath); legacyErr == nil {
			readPath = legacyPath
		}
	}
	data, err := os.ReadFile(readPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read profile notes: %w", err)
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &notes); err != nil {
			return fmt.Errorf("parse profile notes: %w", err)
		}
	}
	a.profileNotes = notes
	a.profileNotesLoaded = true
	return nil
}

func (a *app) persistProfileNotesLocked() error {
	if err := os.MkdirAll(filepath.Dir(a.profileNotesPath), 0o700); err != nil {
		return fmt.Errorf("create profile notes directory: %w", err)
	}
	data, err := json.MarshalIndent(a.profileNotes, "", "  ")
	if err != nil {
		return fmt.Errorf("encode profile notes: %w", err)
	}
	temporary := a.profileNotesPath + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("write profile notes: %w", err)
	}
	if err := os.Rename(temporary, a.profileNotesPath); err != nil {
		return fmt.Errorf("replace profile notes: %w", err)
	}
	return nil
}

func (a *app) listESIMNotes(w http.ResponseWriter, _ *http.Request) {
	notes, err := a.ListESIMNotes()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": notes})
}

func (a *app) saveESIMNote(w http.ResponseWriter, r *http.Request) {
	var body service.ProfileNoteInput
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.SaveESIMNote(body)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func atCommandSucceeded(response string) bool {
	normalized := strings.ReplaceAll(strings.TrimSpace(response), "\r\n", "\n")
	return normalized == "OK" || strings.HasSuffix(normalized, "\nOK")
}

func (a *app) phonebookProbeCommand(command string, result *phonebookProbeResult) bool {
	response, err := a.runATCommand(command, 6*time.Second)
	if err != nil {
		result.Responses[command] = err.Error()
		return false
	}
	result.Responses[command] = strings.TrimSpace(response)
	return atCommandSucceeded(response)
}

// probeESIMPhonebook performs only AT test/read commands. It never writes a
// phonebook entry, so it is safe to use before enabling portable card notes.
func (a *app) probeESIMPhonebook(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.ProbeESIMPhonebook())
}

const moduleNotePrefix = "VH1|"

func encodeModuleProfileNote(note moduleProfileNote) (string, error) {
	note.ICCID = strings.TrimSpace(note.ICCID)
	note.Label = strings.TrimSpace(note.Label)
	note.Phone = strings.TrimSpace(note.Phone)
	note.Tags = strings.TrimSpace(note.Tags)
	if note.ICCID == "" {
		return "", errors.New("iccid is required")
	}
	if len(note.Label) > 48 || len(note.Phone) > 40 || len(note.Tags) > 48 {
		return "", errors.New("模块资料名称、手机号或标签过长")
	}
	encode := func(value string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(value))
	}
	encoded := strings.Join([]string{moduleNotePrefix[:len(moduleNotePrefix)-1], note.ICCID, encode(note.Label), encode(note.Phone), encode(note.Tags)}, "|")
	if len(encoded) > 255 {
		return "", errors.New("模块通讯录记录超过容量")
	}
	return encoded, nil
}

func decodeModuleProfileNote(index int, text string) (moduleProfileNote, bool) {
	parts := strings.Split(text, "|")
	if len(parts) != 5 || parts[0] != strings.TrimSuffix(moduleNotePrefix, "|") || strings.TrimSpace(parts[1]) == "" {
		return moduleProfileNote{}, false
	}
	decode := func(value string) (string, bool) {
		data, err := base64.RawURLEncoding.DecodeString(value)
		return string(data), err == nil
	}
	label, labelOK := decode(parts[2])
	phone, phoneOK := decode(parts[3])
	tags, tagsOK := decode(parts[4])
	if !labelOK || !phoneOK || !tagsOK {
		return moduleProfileNote{}, false
	}
	return moduleProfileNote{Index: index, ICCID: parts[1], Label: label, Phone: phone, Tags: tags}, true
}

func (a *app) runATOK(command string, timeout time.Duration) (string, error) {
	response, err := a.runATCommand(command, timeout)
	if err != nil {
		return "", err
	}
	if !atCommandSucceeded(response) {
		return "", fmt.Errorf("%s: %s", command, strings.TrimSpace(response))
	}
	return response, nil
}

func parseMEPhonebookStatus(response string) (used, total int, err error) {
	re := regexp.MustCompile(`\+CPBS:\s*"ME",(\d+),(\d+)`)
	match := re.FindStringSubmatch(response)
	if len(match) != 3 {
		return 0, 0, errors.New("ME 通讯录容量未返回")
	}
	used, err = strconv.Atoi(match[1])
	if err != nil {
		return 0, 0, err
	}
	total, err = strconv.Atoi(match[2])
	return used, total, err
}

func parseMEPhonebookEntries(response string) []modulePhonebookEntry {
	re := regexp.MustCompile(`(?m)\+CPBR:\s*(\d+),"([^"]*)",\d+,"([^"]*)"`)
	entries := make([]modulePhonebookEntry, 0)
	for _, match := range re.FindAllStringSubmatch(response, -1) {
		index, err := strconv.Atoi(match[1])
		if err == nil {
			entries = append(entries, modulePhonebookEntry{Index: index, Number: match[2], Text: match[3]})
		}
	}
	return entries
}

func (a *app) readModuleESIMNotes() (map[string]moduleProfileNote, map[int]bool, int, int, error) {
	if _, err := a.runATOK(`AT+CPBS="ME"`, 6*time.Second); err != nil {
		return nil, nil, 0, 0, err
	}
	status, err := a.runATOK(`AT+CPBS?`, 6*time.Second)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	used, total, err := parseMEPhonebookStatus(status)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	notes := make(map[string]moduleProfileNote)
	occupied := make(map[int]bool)
	if used == 0 {
		return notes, occupied, used, total, nil
	}
	response, err := a.runATOK(fmt.Sprintf("AT+CPBR=1,%d", total), 20*time.Second)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	for _, entry := range parseMEPhonebookEntries(response) {
		occupied[entry.Index] = true
		if note, ok := decodeModuleProfileNote(entry.Index, entry.Text); ok {
			notes[note.ICCID] = note
		}
	}
	return notes, occupied, used, total, nil
}

func (a *app) listModuleESIMNotes(w http.ResponseWriter, _ *http.Request) {
	result, err := a.ListModuleESIMNotes()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) saveModuleESIMNote(w http.ResponseWriter, r *http.Request) {
	var body moduleProfileNote
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.SaveModuleESIMNote(body)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) esimOverview(w http.ResponseWriter, _ *http.Request) {
	result, err := a.ESIMOverview()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	switch {
	case result.DemoPayload != nil:
		writeJSON(w, http.StatusOK, result.DemoPayload)
	case result.PhysicalSIM:
		writeJSON(w, http.StatusOK, map[string]any{
			"card_type": "physical_sim",
			"message":   result.Message,
		})
	default:
		writeJSON(w, http.StatusOK, result.Overview)
	}
}

// A normal physical SIM cannot open the GSMA eUICC management AIDs. The
// manager reports that as no eUICC discovered with an AT+CCHO ERROR; expose it
// as a neutral card type instead of leaking an implementation error to the UI.
func isPhysicalSIMESIMProbeError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "未发现任何 euicc") &&
		strings.Contains(message, "at+ccho") &&
		strings.Contains(message, "error")
}

func (a *app) esimHealth(w http.ResponseWriter, _ *http.Request) {
	result, err := a.ESIMHealth()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	switch {
	case result.PhysicalSIM:
		writeJSON(w, http.StatusOK, map[string]any{"card_type": "physical_sim"})
	case result.ActiveProfile == nil:
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": result.Message})
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             result.OK,
			"active_profile": result.ActiveProfile,
			"module_iccid":   result.ModuleICCID,
			"imsi":           result.IMSI,
			"operator":       result.Operator,
			"registration":   result.Registration,
			"registered":     result.Registered,
			"signal_dbm":     result.SignalDBM,
			"network_mode":   result.NetworkMode,
		})
	}
}

func (a *app) switchESIM(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ICCID string `json:"iccid"`
		AID   string `json:"aid"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.SwitchESIMProfile(r.Context(), body.ICCID, body.AID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if result.Demo {
		writeJSON(w, http.StatusOK, map[string]any{
			"switch_accepted": result.SwitchAccepted,
			"phase":           result.Phase,
			"target_iccid":    result.TargetICCID,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"switch_accepted":         result.SwitchAccepted,
		"phase":                   result.Phase,
		"target_iccid":            result.TargetICCID,
		"recovery_pending":        result.RecoveryPending,
		"module_reboot_requested": result.ModuleRebootRequested,
		"module_reboot_response":  result.ModuleRebootResponse,
		"module_reboot_warning":   result.ModuleRebootWarning,
		"reconnect_wait_seconds":  result.ReconnectWaitSeconds,
	})
}

func (a *app) renameESIMProfile(w http.ResponseWriter, r *http.Request) {
	esimManager, _ := a.currentESIMManager()
	if !a.demo && esimManager == nil {
		writeError(w, http.StatusServiceUnavailable, "eSIM manager is unavailable")
		return
	}
	var body struct {
		ICCID string `json:"iccid"`
		AID   string `json:"aid"`
		Name  string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.ICCID = strings.TrimSpace(body.ICCID)
	body.Name = strings.TrimSpace(body.Name)
	if body.ICCID == "" || body.Name == "" {
		writeError(w, http.StatusBadRequest, "iccid and name are required")
		return
	}
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]string{"message": "Profile 名称修改成功"})
		return
	}
	if err := esimManager.RenameProfile(body.ICCID, body.Name, body.AID); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("修改 Profile 名称失败: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Profile 名称修改成功"})
}

func (a *app) deleteESIMProfile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ICCID string `json:"iccid"`
		AID   string `json:"aid"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.DeleteESIMProfile(body.ICCID, body.AID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if result.Demo {
		writeJSON(w, http.StatusOK, map[string]string{"message": result.Message})
		return
	}
	writeJSON(w, http.StatusOK, result.Result)
}

func (a *app) downloadESIMProfile(w http.ResponseWriter, r *http.Request) {
	var body service.ESIMDownloadRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.DownloadESIMProfile(r.Context(), body)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if result.Demo {
		writeJSON(w, http.StatusOK, map[string]string{"message": result.Message})
		return
	}
	writeJSON(w, http.StatusOK, result.Result)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
