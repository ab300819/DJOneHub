package main

import (
	"context"
	"fmt"
	"github.com/ab300819/DJOneHub/internal/esim"
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

func (a *app) ESIMOverview() (service.ESIMOverviewResult, error) {
	if a.demo {
		return service.ESIMOverviewResult{DemoPayload: demoESIMOverview()}, nil
	}
	esimManager, _ := a.currentESIMManager()
	if esimManager == nil {
		return service.ESIMOverviewResult{}, service.Fail(service.KindUnavailable, "eSIM manager is unavailable")
	}
	overview, err := esimManager.GetEsimOverview()
	if err != nil {
		if isPhysicalSIMESIMProbeError(err) {
			return service.ESIMOverviewResult{
				PhysicalSIM: true,
				Message:     "当前卡片为实体卡，非 eSIM 卡片",
			}, nil
		}
		log.Printf("eSIM overview failed: %v", err)
		return service.ESIMOverviewResult{}, err
	}
	return service.ESIMOverviewResult{Overview: overview}, nil
}

func (a *app) ESIMHealth() (service.ESIMHealthResult, error) {
	esimManager, _ := a.currentESIMManager()
	if esimManager == nil {
		return service.ESIMHealthResult{}, service.Fail(service.KindUnavailable, "eSIM manager is unavailable")
	}
	overview, err := esimManager.GetEsimOverview()
	if err != nil {
		if isPhysicalSIMESIMProbeError(err) {
			return service.ESIMHealthResult{PhysicalSIM: true}, nil
		}
		return service.ESIMHealthResult{}, err
	}

	var active *esim.ProfileItem
	for _, group := range overview.Profiles {
		for index := range group.Profiles {
			if group.Profiles[index].State == 1 {
				active = &group.Profiles[index]
				break
			}
		}
		if active != nil {
			break
		}
	}
	if active == nil {
		return service.ESIMHealthResult{Message: "eSIM 卡片已识别，但没有已启用的 Profile"}, nil
	}

	if err := a.ensureUSBAT(); err != nil {
		return service.ESIMHealthResult{}, service.Fail(service.KindUnavailable, "%s", err.Error())
	}
	status, err := a.usbATStatus()
	if err != nil {
		a.resetUSBATIfGone(err)
		return service.ESIMHealthResult{}, err
	}
	registered := status.RegStatus == 1 || status.RegStatus == 5
	return service.ESIMHealthResult{
		OK:            status.SimInserted && registered,
		ActiveProfile: active,
		ModuleICCID:   status.ICCID,
		IMSI:          status.IMSI,
		Operator:      status.Operator,
		Registration:  status.RegStatusText,
		Registered:    registered,
		SignalDBM:     status.SignalDBM,
		NetworkMode:   status.NetworkMode,
	}, nil
}

func (a *app) ListESIMNotes() (map[string]service.ProfileNote, error) {
	a.profileNotesMu.Lock()
	defer a.profileNotesMu.Unlock()
	if err := a.loadProfileNotesLocked(); err != nil {
		return nil, service.Fail(service.KindInternal, "%s", err.Error())
	}
	return a.profileNotes, nil
}

func (a *app) SaveESIMNote(input service.ProfileNoteInput) (service.SavedNote, error) {
	iccid := strings.TrimSpace(input.ICCID)
	label := strings.TrimSpace(input.Label)
	phone := strings.TrimSpace(input.Phone)
	tags := strings.TrimSpace(input.Tags)
	if iccid == "" {
		return service.SavedNote{}, service.Fail(service.KindInvalid, "iccid is required")
	}
	if len(label) > 80 || len(phone) > 80 || len(tags) > 200 {
		return service.SavedNote{}, service.Fail(service.KindInvalid, "本地备注字段过长")
	}
	a.profileNotesMu.Lock()
	defer a.profileNotesMu.Unlock()
	if err := a.loadProfileNotesLocked(); err != nil {
		return service.SavedNote{}, service.Fail(service.KindInternal, "%s", err.Error())
	}
	if label == "" && phone == "" && tags == "" {
		delete(a.profileNotes, iccid)
	} else {
		a.profileNotes[iccid] = profileNote{Label: label, Phone: phone, Tags: tags}
	}
	if err := a.persistProfileNotesLocked(); err != nil {
		return service.SavedNote{}, service.Fail(service.KindInternal, "%s", err.Error())
	}
	return service.SavedNote{Message: "本地备注已保存", Note: a.profileNotes[iccid]}, nil
}

func (a *app) ListModuleESIMNotes() (service.ModuleNotes, error) {
	a.moduleNotesMu.Lock()
	defer a.moduleNotesMu.Unlock()
	notes, _, used, total, err := a.readModuleESIMNotes()
	if err != nil {
		return service.ModuleNotes{}, service.Fail(service.KindUpstream, "读取模块资料库失败: %v", err)
	}
	return service.ModuleNotes{Notes: notes, Used: used, Total: total}, nil
}

func (a *app) SaveModuleESIMNote(note service.ModuleProfileNote) (service.ModuleNoteResult, error) {
	a.moduleNotesMu.Lock()
	defer a.moduleNotesMu.Unlock()
	notes, occupied, _, total, err := a.readModuleESIMNotes()
	if err != nil {
		return service.ModuleNoteResult{}, service.Fail(service.KindUpstream, "读取模块资料库失败: %v", err)
	}
	current, exists := notes[strings.TrimSpace(note.ICCID)]
	if strings.TrimSpace(note.Label) == "" && strings.TrimSpace(note.Phone) == "" && strings.TrimSpace(note.Tags) == "" {
		if !exists {
			return service.ModuleNoteResult{Message: "模块资料库中没有此记录"}, nil
		}
		if _, err := a.runATOK(fmt.Sprintf("AT+CPBW=%d", current.Index), 8*time.Second); err != nil {
			return service.ModuleNoteResult{}, service.Fail(service.KindUpstream, "删除模块资料失败: %v", err)
		}
		return service.ModuleNoteResult{Message: "模块资料已删除"}, nil
	}
	encoded, err := encodeModuleProfileNote(note)
	if err != nil {
		return service.ModuleNoteResult{}, service.Fail(service.KindInvalid, "%s", err.Error())
	}
	index := current.Index
	if !exists {
		for candidate := 1; candidate <= total; candidate++ {
			if !occupied[candidate] {
				index = candidate
				break
			}
		}
	}
	if index == 0 {
		return service.ModuleNoteResult{}, service.Fail(service.KindConflict, "模块通讯录已满")
	}
	command := fmt.Sprintf(`AT+CPBW=%d,"00000000000",129,"%s"`, index, encoded)
	if _, err := a.runATOK(command, 8*time.Second); err != nil {
		return service.ModuleNoteResult{}, service.Fail(service.KindUpstream, "写入模块资料失败: %v", err)
	}
	return service.ModuleNoteResult{Message: "模块资料已保存", Index: &index}, nil
}

func (a *app) DownloadESIMProfile(ctx context.Context, request service.ESIMDownloadRequest) (service.ESIMDownloadResult, error) {
	esimManager, _ := a.currentESIMManager()
	if !a.demo && esimManager == nil {
		return service.ESIMDownloadResult{}, service.Fail(service.KindUnavailable, "eSIM manager is unavailable")
	}
	smdp := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(request.SMDP, "https://"), "http://"))
	if smdp == "" {
		return service.ESIMDownloadResult{}, service.Fail(service.KindInvalid, "smdp is required")
	}
	if strings.TrimSpace(request.IMEI) == "" {
		return service.ESIMDownloadResult{}, service.Fail(service.KindInvalid,
			"imei is required for USB AT eSIM download")
	}
	if a.demo {
		return service.ESIMDownloadResult{Demo: true, Message: "演示：Profile 下载完成"}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	result, err := esimManager.DownloadProfile(ctx, request.AID, smdp, request.MatchingID,
		request.ConfirmationCode, request.IMEI, func(event esim.DownloadProgressEvent) {
			log.Printf("eSIM download %d%% %s", event.Pct, event.Msg)
		})
	if err != nil {
		return service.ESIMDownloadResult{}, service.Fail(service.KindUpstream, "下载 Profile 失败: %v", err)
	}
	return service.ESIMDownloadResult{Result: &result}, nil
}

func (a *app) SwitchESIMProfile(ctx context.Context, iccid, aid string) (service.ESIMSwitchResult, error) {
	esimManager, switchAllowed := a.currentESIMManager()
	if !a.demo && esimManager == nil {
		return service.ESIMSwitchResult{}, service.Fail(service.KindUnavailable, "eSIM manager is unavailable")
	}
	if strings.TrimSpace(iccid) == "" {
		return service.ESIMSwitchResult{}, service.Fail(service.KindInvalid, "iccid is required")
	}
	if a.demo {
		return service.ESIMSwitchResult{
			Demo: true, SwitchAccepted: true, Phase: "done", TargetICCID: iccid,
		}, nil
	}
	if !switchAllowed && a.modem == nil {
		return service.ESIMSwitchResult{}, service.Fail(service.KindUnavailable,
			"USB AT eSIM/卡片当前暂不允许切换 Profile")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	result, err := esimManager.SwitchProfileWithResult(ctx, iccid, aid)
	if err != nil {
		return service.ESIMSwitchResult{}, err
	}

	// Enabling a Profile resets the eUICC, but the DJI modem can keep the
	// previous SIM session (and therefore its CNUM) until its own firmware is
	// restarted.  Reload it here so the newly enabled Profile actually becomes
	// the modem's active subscriber identity.
	rebootResponse, rebootErr := a.runATCommand("AT+CFUN=1,1", 3*time.Second)
	rebootRequested := rebootErr == nil
	rebootWarning := ""
	if rebootErr != nil {
		// A USB disconnect immediately after CFUN is expected on some firmware;
		// the command may already have been accepted before the bridge drops.
		upper := strings.ToUpper(rebootErr.Error())
		if strings.Contains(upper, "NO_DEVICE") || strings.Contains(upper, "NOT_FOUND") {
			rebootRequested = true
		} else {
			rebootWarning = rebootErr.Error()
			log.Printf("eSIM profile switched but module restart was not confirmed: %v", rebootErr)
		}
	}
	return service.ESIMSwitchResult{
		SwitchAccepted:        result.SwitchAccepted,
		Phase:                 string(result.Phase),
		TargetICCID:           result.TargetICCID,
		RecoveryPending:       result.RecoveryPending,
		ModuleRebootRequested: rebootRequested,
		ModuleRebootResponse:  rebootResponse,
		ModuleRebootWarning:   rebootWarning,
		ReconnectWaitSeconds:  10,
	}, nil
}

func (a *app) DeleteESIMProfile(iccid, aid string) (service.ESIMDeleteResult, error) {
	esimManager, _ := a.currentESIMManager()
	if !a.demo && esimManager == nil {
		return service.ESIMDeleteResult{}, service.Fail(service.KindUnavailable, "eSIM manager is unavailable")
	}
	iccid = strings.TrimSpace(iccid)
	if iccid == "" {
		return service.ESIMDeleteResult{}, service.Fail(service.KindInvalid, "iccid is required")
	}
	if a.demo {
		return service.ESIMDeleteResult{Demo: true, Message: "Profile 已删除"}, nil
	}
	result, err := esimManager.DeleteProfile(iccid, aid)
	if err != nil {
		return service.ESIMDeleteResult{}, service.Fail(service.KindUpstream, "删除 Profile 失败: %v", err)
	}
	return service.ESIMDeleteResult{Result: &result}, nil
}

func (a *app) ProbeESIMPhonebook() service.PhonebookProbe {
	result := service.PhonebookProbe{Responses: make(map[string]string)}
	if a.demo {
		result.StorageSupported = true
		result.StorageSelected = true
		result.ReadSupported = true
		result.WriteSupported = true
		result.StorageStatus = `+CPBS: "SM",0,250`
		result.Responses[`AT+CPBS=?`] = `+CPBS: ("SM","ME")\r\nOK`
		result.Responses[`AT+CPBR=?`] = `+CPBR: (1-250),40,14\r\nOK`
		result.Responses[`AT+CPBW=?`] = `+CPBW: (1-250),40,(129,145),16\r\nOK`
		return result
	}
	if !a.phonebookProbeCommand(`AT+CPBS=?`, &result) {
		return result
	}
	result.StorageSupported = strings.Contains(strings.ToUpper(result.Responses[`AT+CPBS=?`]), `"SM"`)
	if !result.StorageSupported {
		return result
	}
	result.StorageSelected = a.phonebookProbeCommand(`AT+CPBS="SM"`, &result)
	if !result.StorageSelected {
		return result
	}
	if a.phonebookProbeCommand(`AT+CPBS?`, &result) {
		result.StorageStatus = result.Responses[`AT+CPBS?`]
	}
	result.ReadSupported = a.phonebookProbeCommand(`AT+CPBR=?`, &result)
	result.WriteSupported = a.phonebookProbeCommand(`AT+CPBW=?`, &result)
	return result
}

// demoESIMOverview is fixture data shaped like a real overview response. It is
// built as a literal rather than an esim.EsimOverview so demo output stays
// exactly what it has always been, with no zero-valued fields appearing.
func demoESIMOverview() map[string]any {
	return map[string]any{
		"chip_info": map[string]any{
			"sku_name":      "eUICC Demo Card",
			"serial_number": "DEMO-001",
			"firmware":      "1.0.0",
			"eids": []map[string]any{{
				"aid": "A0000005591010FFFFFFFF8900000100",
				"eid": "89049032000000000000000000000001",
			}},
		},
		"profiles": []map[string]any{{
			"eid":     "89049032000000000000000000000001",
			"aid_hex": "A0000005591010FFFFFFFF8900000100",
			"profiles": []map[string]any{
				{
					"iccid": "89860123456789012345", "name": "中国移动",
					"service_provider_name": "China Mobile", "state": 1, "state_text": "enabled",
				},
				{
					"iccid": "8944100000000000001", "name": "英国旅行卡",
					"service_provider_name": "giffgaff UK", "state": 0, "state_text": "disabled",
				},
				{
					"iccid": "8949020000000000002", "name": "欧洲数据卡",
					"service_provider_name": "Travel Europe", "state": 0, "state_text": "disabled",
				},
			},
		}},
	}
}
