package main

import (
	"context"
	"embed"
	"flag"
	"log"
	"os"
	"strings"
	"time"

	"github.com/ab300819/DJOneHub/core"
	"github.com/ab300819/DJOneHub/internal/backend"
	"github.com/ab300819/DJOneHub/internal/config"
	"github.com/ab300819/DJOneHub/internal/esim"
	"github.com/ab300819/DJOneHub/internal/modem"
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
	flag.IntVar(&parentPID, "parent-pid", 0, "exit when this parent process goes away; used by the macOS core.App")
	flag.BoolVar(&stdioMode, "stdio", false, "serve line-delimited JSON on stdin/stdout instead of HTTP")
	flag.Parse()

	probe := defaultHostProbe()
	if demo {
		instance := core.NewDemo(probe)
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
			instance := core.New(core.Options{
				Host:            probe,
				ATTransport:     usbATDevice,
				OpenATTransport: openDJIUSBAT,
				Port:            "未发现 AT 串口",
				DiscoveryError:  err.Error(),
				USBDevice:       usbDevice,
			})
			if usbDevice != nil {
				log.Printf("DJI USB device detected without AT serial port: %s %s (%s:%s)",
					usbDevice.Vendor, usbDevice.Product, usbDevice.VendorID, usbDevice.ProductID)
			}
			if usbATErr != nil {
				log.Printf("USB AT unavailable: %v", usbATErr)
			} else {
				instance.SetPort(usbATDevice.Description())
				defer usbATDevice.Close()
				log.Printf("USB AT bridge opened on DJI %s", usbATDevice.Description())
				instance.InitUSBATESIMManager()
			}
			log.Printf("modem discovery skipped: %v", err)
			go instance.StartSMSPoller(context.Background())
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

	instance := core.New(core.Options{Modem: manager, Host: probe, Port: port})
	manager.SetSMSCallback(instance.RecordSMS)
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
		instance.InstallESIMManager(esimManager, false)
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
