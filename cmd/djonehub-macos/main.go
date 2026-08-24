package main

import (
	"context"
	"embed"
	"flag"
	"log"
	"os"
	"strings"
	"time"

	"github.com/ab300819/DJOneHub/internal/backend"
	"github.com/ab300819/DJOneHub/internal/config"
	"github.com/ab300819/DJOneHub/internal/esim"
	"github.com/ab300819/DJOneHub/internal/modem"
	"github.com/ab300819/DJOneHub/pkg/smscodec"
)

//go:embed web/*
var webAssets embed.FS

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

	probe := defaultHostProbe()
	if demo {
		instance := newDemoApp(probe)
		log.Printf("DJOneHub demo mode")
		serve(instance, listen)
		return
	}

	if strings.TrimSpace(port) == "" {
		var err error
		port, err = probe.ATPort()
		if err != nil {
			usbDevice := probe.USBDevice()
			usbATDevice, usbATErr := openDJIUSBAT()
			instance := &app{
				port:             "未发现 AT 串口",
				discoveryError:   err.Error(),
				usbDevice:        usbDevice,
				usbAT:            usbATDevice,
				host:             probe,
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

	instance := &app{
		modem:            manager,
		host:             probe,
		port:             port,
		smsPollInterval:  8 * time.Second,
		smsAutoCleanupME: true,
	}
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
