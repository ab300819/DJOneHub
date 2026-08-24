package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeATTransport answers from a table instead of from hardware. It is the
// reason ATTransport exists: every test below reaches code that previously
// needed a module on the end of a USB cable.
type fakeATTransport struct {
	responses map[string]string
	// promptReply answers any prompt command the table does not name, so a test
	// can exercise multi-segment SMS without predicting each PDU's length.
	promptReply string
	errs        map[string]error
	issued      []string
	prompted    [][]byte
	closed      bool
}

func (f *fakeATTransport) Command(cmd string, _ time.Duration) (string, error) {
	f.issued = append(f.issued, cmd)
	if err := f.errs[cmd]; err != nil {
		return "", err
	}
	resp, ok := f.responses[cmd]
	if !ok {
		return "", errors.New("unexpected command: " + cmd)
	}
	return resp, nil
}

func (f *fakeATTransport) CommandWithPrompt(cmd string, followUp []byte, _ time.Duration) (string, error) {
	f.issued = append(f.issued, cmd)
	f.prompted = append(f.prompted, followUp)
	if err := f.errs[cmd]; err != nil {
		return "", err
	}
	if resp, ok := f.responses[cmd]; ok {
		return resp, nil
	}
	if f.promptReply != "" {
		return f.promptReply, nil
	}
	return "", errors.New("unexpected prompt command: " + cmd)
}

func (f *fakeATTransport) Close()              { f.closed = true }
func (f *fakeATTransport) Description() string { return "fake" }

// moduleResponses mirrors what the QDC507 actually answers when a SIM is
// registered on LTE.
func moduleResponses() map[string]string {
	return map[string]string{
		"ATI":              "ATI\r\nQuectel\r\nEG25\r\nRevision: QDC507GLEFM21\r\n\r\nOK",
		"AT+CPIN?":         "+CPIN: READY\r\n\r\nOK",
		"AT+CSQ":           "+CSQ: 24,99\r\n\r\nOK",
		"AT+CEREG?":        "+CEREG: 0,1\r\n\r\nOK",
		"AT+CREG?":         "+CREG: 0,0\r\n\r\nOK",
		"AT+COPS=3,2":      "OK",
		"AT+COPS?":         "+COPS: 0,0,\"CHN-UNICOM\",7\r\n\r\nOK",
		"AT+QCCID":         "+QCCID: 89860112345678901234\r\n\r\nOK",
		"AT+CIMI":          "460019876543210\r\n\r\nOK",
		"AT+QNWINFO":       "+QNWINFO: \"FDD LTE\",\"46001\",\"LTE BAND 3\",1650\r\n\r\nOK",
		`AT+QCFG="usbnet"`: "+QCFG: \"usbnet\",1\r\n\r\nOK",
		"AT+CGSN":          "AT+CGSN\r\n860000000000001\r\n\r\nOK",
		"AT+CGATT?":        "+CGATT: 1\r\n\r\nOK",
	}
}

func TestUSBATStatusParsesAModuleAnswer(t *testing.T) {
	transport := &fakeATTransport{responses: moduleResponses()}
	instance := &app{usbAT: transport}

	status, err := instance.usbATStatus()
	if err != nil {
		t.Fatalf("usbATStatus() error = %v", err)
	}

	if !strings.Contains(status.Firmware, "QDC507GLEFM21") {
		t.Errorf("Firmware = %q, want it to name the module revision", status.Firmware)
	}
	if !status.SimInserted {
		t.Error("SimInserted = false, want true for a READY SIM")
	}
	if status.SignalDBM != -65 {
		t.Errorf("SignalDBM = %d, want -65 for CSQ 24", status.SignalDBM)
	}
	if status.RegStatus != 1 {
		t.Errorf("RegStatus = %d, want 1 from CEREG when CREG reports 0", status.RegStatus)
	}
	if status.Operator != "CHN-UNICOM" {
		t.Errorf("Operator = %q, want CHN-UNICOM", status.Operator)
	}
	if status.ICCID != "89860112345678901234" {
		t.Errorf("ICCID = %q", status.ICCID)
	}
	if status.IMSI != "460019876543210" {
		t.Errorf("IMSI = %q", status.IMSI)
	}
	// QNWINFO is split apart on the way in: the duplex mode leaves the
	// technology name, and the band drops its redundant "LTE " prefix.
	if status.NetworkMode != "LTE" || status.NetworkDuplex != "FDD" ||
		status.RadioBand != "BAND 3" || status.RadioChannel != 1650 {
		t.Errorf("QNWINFO parsed as mode=%q duplex=%q band=%q channel=%d",
			status.NetworkMode, status.NetworkDuplex, status.RadioBand, status.RadioChannel)
	}
	if status.USBNetMode != 1 {
		t.Errorf("USBNetMode = %d, want 1 (ECM)", status.USBNetMode)
	}
	if status.IMEI != "860000000000001" {
		t.Errorf("IMEI = %q", status.IMEI)
	}
	// Registered and attached are separate states, and the view used to report
	// the second one as false no matter what the module said.
	if !status.PSAttached {
		t.Error("PSAttached = false although AT+CGATT? answered 1")
	}
}

// Only AT+CPIN? aborts the status read; the other commands are best-effort. A
// module that answers nothing else should still report what it can.
func TestUSBATStatusFailsOnlyWhenCPINFails(t *testing.T) {
	transport := &fakeATTransport{
		responses: moduleResponses(),
		errs:      map[string]error{"AT+CPIN?": errors.New("USB AT command timed out")},
	}
	instance := &app{usbAT: transport}

	if _, err := instance.usbATStatus(); err == nil {
		t.Fatal("usbATStatus() succeeded despite a failing AT+CPIN?")
	}

	transport = &fakeATTransport{
		responses: moduleResponses(),
		errs:      map[string]error{"AT+QNWINFO": errors.New("USB AT command timed out")},
	}
	instance = &app{usbAT: transport}
	status, err := instance.usbATStatus()
	if err != nil {
		t.Fatalf("a failing AT+QNWINFO should not abort the status read: %v", err)
	}
	if status.NetworkMode != "" {
		t.Errorf("NetworkMode = %q, want empty when QNWINFO failed", status.NetworkMode)
	}
	if status.ICCID == "" {
		t.Error("the rest of the status was discarded along with QNWINFO")
	}
}

// A removed module surfaces as NO_DEVICE from libusb. The core has to drop the
// handle so a later poll can reopen a freshly enumerated one.
func TestRunATCommandDropsTheTransportWhenTheModuleIsGone(t *testing.T) {
	transport := &fakeATTransport{
		responses: map[string]string{},
		errs:      map[string]error{"ATI": errors.New("USB bulk write: LIBUSB_ERROR_NO_DEVICE")},
	}
	instance := &app{usbAT: transport, usbDevice: &usbDeviceStatus{}}

	if _, err := instance.runATCommand("ATI", time.Second); err == nil {
		t.Fatal("runATCommand() succeeded despite NO_DEVICE")
	}
	if instance.usbAT != nil {
		t.Error("the dead transport was kept; a reopen can never happen")
	}
	if !transport.closed {
		t.Error("the dead transport was not closed")
	}
}

// An unrelated error must not cost us a working handle.
func TestRunATCommandKeepsTheTransportOnAnOrdinaryError(t *testing.T) {
	transport := &fakeATTransport{
		responses: map[string]string{"AT+BOGUS": "ERROR"},
	}
	instance := &app{usbAT: transport, usbDevice: &usbDeviceStatus{}}

	if _, err := instance.runATCommand("AT+BOGUS", time.Second); err != nil {
		t.Fatalf("an ERROR answer is not a transport failure: %v", err)
	}
	if instance.usbAT == nil {
		t.Error("a working transport was discarded")
	}
}

// SMS submission is a two-step exchange: AT+CMGS=<len> draws a prompt, then the
// hex PDU plus Ctrl-Z goes out. Long messages split into several such rounds.
func TestSendUSBATSMSSubmitsOnePDUPerSegment(t *testing.T) {
	transport := &fakeATTransport{
		responses: map[string]string{
			"AT+CMGF=0":  "OK",
			"AT+CMGS=16": "+CMGS: 42\r\n\r\nOK",
		},
	}
	instance := &app{usbAT: transport, usbDevice: &usbDeviceStatus{}}

	sent, err := instance.sendUSBATSMS("+8613800138000", "hi")
	if err != nil {
		t.Fatalf("sendUSBATSMS() error = %v (issued %v)", err, transport.issued)
	}
	if sent != 1 {
		t.Errorf("sent = %d segments, want 1 for a short message", sent)
	}
	if len(transport.prompted) != 1 {
		t.Fatalf("prompted %d times, want 1", len(transport.prompted))
	}
	payload := string(transport.prompted[0])
	if !strings.HasSuffix(payload, "\x1a") {
		t.Error("the PDU payload does not end with Ctrl-Z, so the module never submits")
	}
	if strings.ToUpper(payload) != payload {
		t.Errorf("the PDU is not upper-case hex: %q", payload)
	}
}

// A message past one PDU's capacity goes out as several submissions, and the
// caller is told how many.
func TestSendUSBATSMSSplitsALongMessage(t *testing.T) {
	transport := &fakeATTransport{
		responses:   map[string]string{"AT+CMGF=0": "OK"},
		promptReply: "+CMGS: 42\r\n\r\nOK",
	}
	instance := &app{usbAT: transport, usbDevice: &usbDeviceStatus{}}

	sent, err := instance.sendUSBATSMS("+8613800138000", strings.Repeat("a", 200))
	if err != nil {
		t.Fatalf("sendUSBATSMS() error = %v (issued %v)", err, transport.issued)
	}
	if sent != 2 {
		t.Errorf("sent = %d segments, want 2 for a 200-character message", sent)
	}
	if len(transport.prompted) != sent {
		t.Errorf("prompted %d times for %d reported segments", len(transport.prompted), sent)
	}
}

func TestSendUSBATSMSReportsARefusedPDUMode(t *testing.T) {
	transport := &fakeATTransport{responses: map[string]string{"AT+CMGF=0": "ERROR"}}
	instance := &app{usbAT: transport, usbDevice: &usbDeviceStatus{}}

	if _, err := instance.sendUSBATSMS("+8613800138000", "hi"); err == nil {
		t.Fatal("sendUSBATSMS() succeeded although the module refused PDU mode")
	}
}

func TestParseUSBATCGATTReadsTheAttachState(t *testing.T) {
	for _, tt := range []struct {
		name     string
		response string
		want     bool
	}{
		{name: "attached", response: "AT+CGATT?\r\n+CGATT: 1\r\n\r\nOK", want: true},
		{name: "not attached", response: "AT+CGATT?\r\n+CGATT: 0\r\n\r\nOK", want: false},
		{name: "refused", response: "AT+CGATT?\r\nERROR", want: false},
		{name: "empty", response: "", want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseUSBATCGATT(tt.response); got != tt.want {
				t.Fatalf("parseUSBATCGATT(%q) = %v, want %v", tt.response, got, tt.want)
			}
		})
	}
}

// AT+CGSN answers with a bare IMEI and AT+CIMI with a bare IMSI, which is why
// one reader serves both.
func TestParseUSBATBareDigitsReadsIMEIAndIMSI(t *testing.T) {
	if got := parseUSBATBareDigits("AT+CGSN\r\n860000000000001\r\n\r\nOK"); got != "860000000000001" {
		t.Errorf("IMEI = %q", got)
	}
	if got := parseUSBATBareDigits("AT+CIMI\r\n460115089094752\r\n\r\nOK"); got != "460115089094752" {
		t.Errorf("IMSI = %q", got)
	}
	if got := parseUSBATBareDigits("AT+CGSN\r\nERROR"); got != "" {
		t.Errorf("a refused query returned %q, want empty", got)
	}
}
