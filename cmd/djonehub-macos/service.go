package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
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

func (a *app) NetworkDiagnostic() (result service.NetworkDiagnostic, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("network diagnostic panic: %v", recovered)
			err = service.Fail(service.KindInternal, "network diagnostic failed: %v", recovered)
		}
	}()
	raw := make(map[string]string)
	errs := make(map[string]string)
	diag := service.NetworkDiagnostic{
		USBDevice:     a.currentUSBDevice(),
		MacInterfaces: discoverMacNetworkInterfaces(),
		DefaultRoute:  discoverMacDefaultRoute(),
		Raw:           raw,
		Errors:        errs,
	}
	diag.USBNetworkPresent = hasLikelyUSBNetworkInterface(diag.MacInterfaces)

	commands := map[string]string{
		"usbnet":  `AT+QCFG="usbnet"`,
		"usbcfg":  `AT+QCFG="usbcfg"`,
		"cgdcont": `AT+CGDCONT?`,
		"cgact":   `AT+CGACT?`,
		"cgpaddr": `AT+CGPADDR=1`,
	}
	for key, command := range commands {
		resp, cmdErr := a.runATCommand(command, 8*time.Second)
		if cmdErr != nil {
			errs[key] = cmdErr.Error()
			continue
		}
		raw[key] = resp
	}

	diag.USBNetMode = parseUSBNetMode(raw["usbnet"])
	diag.USBCfg = parseUSBATPrefixed(raw["usbcfg"], "+QCFG:")
	diag.PDPContexts = parsePDPContexts(raw["cgdcont"])
	diag.ActiveContexts = parseActivePDPContexts(raw["cgact"])
	diag.PDPAddresses = parsePDPAddresses(raw["cgpaddr"])
	if len(errs) == 0 {
		diag.Errors = nil
	}
	return diag, nil
}

func (a *app) NetworkTraffic() service.TrafficSnapshot {
	snapshot := service.TrafficSnapshot{SampledAtMS: time.Now().UnixMilli()}

	interfaces := discoverMacNetworkInterfaces()
	name := selectUSBTrafficInterface(interfaces, discoverMacDefaultRoute())
	if name == "" {
		return snapshot
	}
	counters, err := discoverMacInterfaceCounters()
	if err != nil {
		snapshot.Interface = name
		snapshot.Error = err.Error()
		return snapshot
	}
	current, ok := counters[name]
	if !ok {
		snapshot.Interface = name
		snapshot.Error = "未读取到网卡计数"
		return snapshot
	}

	a.trafficMu.Lock()
	if a.trafficBaselines == nil {
		a.trafficBaselines = make(map[string]networkByteCounters)
	}
	baseline, exists := a.trafficBaselines[name]
	if !exists || current.RX < baseline.RX || current.TX < baseline.TX {
		baseline = current
		a.trafficBaselines[name] = baseline
	}
	a.trafficMu.Unlock()

	snapshot.Available = true
	snapshot.Interface = name
	snapshot.RXBytes = current.RX
	snapshot.TXBytes = current.TX
	snapshot.SessionRX, snapshot.SessionTX, snapshot.SessionTotal = sessionTrafficFromCounters(current, baseline)
	return snapshot
}

func (a *app) Check4GRoute() service.NetworkCheckResult {
	route := discoverMacDefaultRoute()
	interfaces := discoverMacNetworkInterfaces()
	var active *service.MacNetInterface
	for i := range interfaces {
		if interfaces[i].Name == route.Interface {
			active = &interfaces[i]
			break
		}
	}
	if route.Interface == "" {
		return service.NetworkCheckResult{
			OK:      false,
			Summary: "未读取到默认出口",
			Detail:  "macOS 没有返回 default route",
		}
	}
	if active != nil && active.Name != "en0" && active.Kind == "ethernet" && active.Status == "active" {
		return service.NetworkCheckResult{
			OK:      true,
			Summary: "当前正在走 4G 模块",
			Detail:  fmt.Sprintf("默认出口 %s -> %s，IP %s", route.Interface, route.Gateway, active.IPv4),
		}
	}
	detail := fmt.Sprintf("默认出口 %s -> %s", route.Interface, route.Gateway)
	if active != nil && active.IPv4 != "" {
		detail += "，IP " + active.IPv4
	}
	return service.NetworkCheckResult{OK: false, Summary: "当前没有优先走 4G 模块", Detail: detail}
}

func (a *app) CheckProxyRoute() service.NetworkCheckResult {
	proxyURL, _ := url.Parse("http://127.0.0.1:7890")
	client := &http.Client{
		Timeout:   8 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}
	req, err := http.NewRequest(http.MethodHead, "https://www.google.com/generate_204", nil)
	if err != nil {
		return service.NetworkCheckResult{OK: false, Summary: "代理检测请求创建失败", Detail: err.Error()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return service.NetworkCheckResult{
			OK:      false,
			Summary: "代理未打通",
			Detail:  "127.0.0.1:7890 代理访问失败：" + err.Error(),
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || (resp.StatusCode >= 200 && resp.StatusCode < 400) {
		return service.NetworkCheckResult{
			OK:      true,
			Summary: "代理已打通",
			Detail:  fmt.Sprintf("127.0.0.1:7890 -> google generate_204 返回 %s", resp.Status),
		}
	}
	return service.NetworkCheckResult{
		OK:      false,
		Summary: "代理响应异常",
		Detail:  fmt.Sprintf("127.0.0.1:7890 返回 %s", resp.Status),
	}
}

func (a *app) SetUSBNetMode(mode int) (service.USBNetResult, error) {
	if mode < 0 || mode > 3 {
		return service.USBNetResult{}, service.Fail(service.KindInvalid,
			"only usbnet mode 0, 1, 2 or 3 is allowed")
	}
	response, err := a.runATCommand(fmt.Sprintf(`AT+QCFG="usbnet",%d`, mode), 8*time.Second)
	if err != nil {
		return service.USBNetResult{}, err
	}
	return service.USBNetResult{Mode: mode, Response: response, NeedsReboot: true}, nil
}

func (a *app) RebootModule() (service.RebootResult, error) {
	response, err := a.runATCommand("AT+CFUN=1,1", 3*time.Second)
	if err != nil {
		return service.RebootResult{}, err
	}
	return service.RebootResult{Accepted: true, Response: response}, nil
}
