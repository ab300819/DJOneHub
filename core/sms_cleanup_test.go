package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Clearing the module's copy is the first link in the loss chain: once
// AT+CMGD runs, the module's messages are gone and ours is the only copy left.
// These cover when that is allowed to happen.

// A single unread message. The PDU has to be a real one: an undecodable body
// falls back to stamping time.Now(), which makes the same message read from SM
// and from ME look like two.
const onePDUMessage = "+CMGL: 1,0,,38\r\n" +
	"079144872000302320048102020000625061028204401AD9775D0E72D7DBE2B21C949E8360B75A4E7683D16AB71B" +
	"\r\nOK"

func cleanupApp(t *testing.T, historyPath string, keepModuleCopy bool) (*App, *fakeATTransport) {
	t.Helper()
	transport := &fakeATTransport{responses: map[string]string{
		"AT+CMGF=0":              "OK",
		`AT+CPMS="SM","SM","SM"`: "+CPMS: 0,50,0,50,0,50\r\nOK",
		`AT+CPMS="ME","ME","ME"`: "+CPMS: 1,50,1,50,1,50\r\nOK",
		"AT+CMGL=4":              onePDUMessage,
		"AT+CMGD=1,4":            "OK",
	}}
	app := New(Options{Host: UnsupportedHost{}, ATTransport: transport, KeepModuleCopy: keepModuleCopy})
	app.smsHistoryPath = historyPath
	return app, transport
}

func cleared(transport *fakeATTransport) bool {
	for _, cmd := range transport.issued {
		if cmd == "AT+CMGD=1,4" {
			return true
		}
	}
	return false
}

// The default is what every existing install runs, so it has to stay the
// default rather than quietly flipping when the switch was added.
func TestModuleCopyIsClearedByDefault(t *testing.T) {
	app, transport := cleanupApp(t, filepath.Join(t.TempDir(), "sms.jsonl"), false)

	if err := app.pollSMSOnce(); err != nil {
		t.Fatalf("poll failed: %v", err)
	}
	if !cleared(transport) {
		t.Fatal("默认配置下没有清理模块副本")
	}
	if !app.SMSStatus().AutoCleanupME {
		t.Error("SMSStatus 未报告自动清理已开启")
	}
}

func TestKeepingTheModuleCopyLeavesItAlone(t *testing.T) {
	app, transport := cleanupApp(t, filepath.Join(t.TempDir(), "sms.jsonl"), true)

	if err := app.pollSMSOnce(); err != nil {
		t.Fatalf("poll failed: %v", err)
	}
	if cleared(transport) {
		t.Fatal("已要求保留模块副本，却仍然下发了 AT+CMGD")
	}
	if app.SMSStatus().AutoCleanupME {
		t.Error("SMSStatus 仍报告自动清理开启")
	}
	// Reading has to keep working — the point is to keep the module's copy,
	// not to stop collecting.
	if len(app.ListSMS()) != 1 {
		t.Errorf("读到 %d 条，保留模块副本不应影响收取", len(app.ListSMS()))
	}
}

// Deleting the module's copy before ours is safely written destroys the last
// copy. The write failing is exactly when the module's copy matters most.
func TestAFailedWriteStopsTheModuleCopyFromBeingCleared(t *testing.T) {
	// A directory where the history file should be: opening it for append fails.
	dir := t.TempDir()
	blocked := filepath.Join(dir, "sms.jsonl")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatalf("准备不可写路径失败: %v", err)
	}

	app, transport := cleanupApp(t, blocked, false)

	if err := app.pollSMSOnce(); err != nil {
		t.Fatalf("poll failed: %v", err)
	}
	if cleared(transport) {
		t.Fatal("落盘失败时仍清空了模块副本——最后一份拷贝被销毁")
	}
}

// A read that finds nothing must not clear either: there is no new copy to
// have made, and clearing would discard whatever the module still held.
func TestNothingNewLeavesTheModuleCopyAlone(t *testing.T) {
	app, transport := cleanupApp(t, filepath.Join(t.TempDir(), "sms.jsonl"), false)
	transport.responses["AT+CMGL=4"] = "OK"

	if err := app.pollSMSOnce(); err != nil {
		t.Fatalf("poll failed: %v", err)
	}
	if cleared(transport) {
		t.Fatal("没有读到新消息却清空了模块副本")
	}
}

// The list is capped so a poll does not ship the whole archive over the pipe
// every few seconds. Silently showing 500 of 3000 is what AC-023 forbids.
func TestStatusReportsWhatTheListDoesNotShow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sms.jsonl")
	var lines strings.Builder
	for i := range 520 {
		lines.WriteString(`{"sender":"10086","content":"第` + itoa(i) + `条","timestamp":"2026-09-15T10:` +
			pad(i%60) + `:` + pad(i/60) + `Z"}` + "\n")
	}
	if err := os.WriteFile(path, []byte(lines.String()), 0o600); err != nil {
		t.Fatalf("写历史失败: %v", err)
	}

	app := New(Options{Host: UnsupportedHost{}})
	app.smsHistoryPath = path
	status := app.SMSStatus()

	if status.Count != 500 {
		t.Errorf("Count = %d，列表上限应为 500", status.Count)
	}
	if status.Stored != 520 {
		t.Errorf("Stored = %d，想要 520——截断必须对用户可见", status.Stored)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func pad(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}

var _ = errors.New
