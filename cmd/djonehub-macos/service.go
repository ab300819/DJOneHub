package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ab300819/DJOneHub/internal/modem"
	"github.com/ab300819/DJOneHub/internal/service"
)

// app satisfies the transport-independent surface, so the HTTP handlers and the
// stdio bridge share one implementation instead of each reaching into the modem
// packages on their own.
var _ service.Service = (*app)(nil)

func (a *app) Health() service.Health {
	esimManager, _ := a.currentESIMManager()
	return service.Health{
		OK:             true,
		Port:           a.port,
		ESIMAvailable:  a.demo || esimManager != nil,
		Demo:           a.demo,
		USBDevice:      a.currentUSBDevice(),
		DiscoveryError: a.discoveryError,
	}
}

func (a *app) Status() (service.Status, error) {
	if a.demo {
		return service.Status{Device: &modem.DeviceStatus{
			IMEI:          "867400000000001",
			Firmware:      "EG25GGBR07A08M2G",
			ICCID:         "89860123456789012345",
			IMSI:          "460001234567890",
			Operator:      "China Mobile",
			SimInserted:   true,
			SignalDBM:     -73,
			SignalRSRP:    -96,
			SignalRSRQ:    -9,
			RegStatus:     1,
			RegStatusText: "已注册",
			NetworkMode:   "LTE",
			NetworkDuplex: "FDD",
			RadioBand:     "B3",
			USBNetMode:    0,
		}}, nil
	}

	if a.modem == nil {
		// A libusb handle may survive a physical unplug. Refresh the macOS USB
		// inventory before using it so the UI never reports a stale connection.
		if a.usbAT != nil && a.currentUSBDevice() == nil {
			a.markUSBATDetached("DJI USB device disconnected")
		}
		if err := a.ensureUSBAT(); err != nil {
			log.Printf("USB AT retry failed: %v", err)
		}
		if a.usbAT != nil {
			status, err := a.usbATStatus()
			if err == nil {
				return service.Status{Device: &status}, nil
			}
			a.resetUSBATIfGone(err)
			log.Printf("USB AT status failed: %v", err)
		}

		usbDevice := a.currentUSBDevice()
		degraded := service.DegradedStatus{
			Operator:       "未连接",
			NetworkMode:    "不可用",
			HardwareStatus: "未发现 AT 串口",
			DiscoveryError: a.discoveryError,
			USBDevice:      usbDevice,
		}
		if usbDevice != nil {
			degraded.HardwareStatus = fmt.Sprintf("%s %s (%s:%s)",
				usbDevice.Vendor, usbDevice.Product, usbDevice.VendorID, usbDevice.ProductID)
			degraded.Operator = "已检测到 USB 设备"
			degraded.NetworkMode = usbDevice.Mode
		}
		return service.Status{Degraded: &degraded}, nil
	}

	full := a.modem.GetFullStatus()
	return service.Status{Device: &full}, nil
}

func (a *app) ExecuteAT(command string) (string, error) {
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(command)), "AT") {
		return "", service.ErrInvalidCommand
	}
	response, err := a.runATCommand(command, 20*time.Second)
	if a.demo {
		// Demo mode answers from a fixed table and reports no transport errors.
		return response, nil
	}
	return response, err
}

func (a *app) ListSMS() []service.ReceivedSMS {
	a.smsMu.RLock()
	items := append([]receivedSMS(nil), a.sms...)
	a.smsMu.RUnlock()
	if items == nil {
		items = []receivedSMS{}
	}
	return items
}

func (a *app) SMSStatus() service.SMSStatus {
	a.smsMu.RLock()
	lastPoll := a.smsLastPoll
	lastPollError := a.smsLastPollError
	count := len(a.sms)
	a.smsMu.RUnlock()
	return service.SMSStatus{
		Count:         count,
		Polling:       !a.demo && a.modem == nil,
		PollIntervalS: int(a.smsPollInterval.Seconds()),
		AutoCleanupME: a.smsAutoCleanupME,
		LastPoll:      lastPoll,
		LastPollError: lastPollError,
	}
}

func (a *app) RefreshSMS() (service.RefreshResult, error) {
	if a.demo {
		return service.RefreshResult{Accepted: true}, nil
	}
	if a.modem == nil {
		if err := a.pollSMSOnce(); err != nil {
			return service.RefreshResult{}, err
		}
		a.smsMu.RLock()
		count := len(a.sms)
		a.smsMu.RUnlock()
		return service.RefreshResult{Accepted: true, Count: &count}, nil
	}
	go a.modem.CheckAllSMS()
	return service.RefreshResult{Accepted: true}, nil
}

func (a *app) ClearModuleSMS() (service.ClearResult, error) {
	if a.demo {
		return service.ClearResult{Cleared: true}, nil
	}
	if a.modem != nil {
		return service.ClearResult{}, service.Fail(service.KindUnavailable,
			"module SMS cleanup is only available through USB AT")
	}
	if err := a.ensureUSBAT(); err != nil {
		return service.ClearResult{}, service.Fail(service.KindUnavailable,
			"AT serial port is unavailable: %s", err.Error())
	}
	before, after, err := a.clearUSBATSMSMemory("ME")
	if err != nil {
		return service.ClearResult{}, err
	}
	return service.ClearResult{Cleared: true, Memory: "ME", Before: before, After: after}, nil
}

func (a *app) SendSMS(phone, message string) (service.SendResult, error) {
	if strings.TrimSpace(phone) == "" || strings.TrimSpace(message) == "" {
		return service.SendResult{}, service.Fail(service.KindInvalid, "phone and message are required")
	}
	if a.demo {
		a.recordSMS("已发送至 "+phone, message, time.Now())
		return service.SendResult{Sent: true, Segments: 1}, nil
	}
	segments, err := a.sendTextSMS(phone, message)
	if err != nil {
		return service.SendResult{}, err
	}
	return service.SendResult{Sent: true, Segments: segments}, nil
}
