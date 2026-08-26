package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The module deletes its copy once it has been read, so the core holds the only
// one. It runs as a child of the app and dies with it, which until now meant
// every message was lost on quit.
//
// @verifies AC-137, AC-138, AC-139, AC-140
// @testcase UT-050

func historyApp(t *testing.T, path string) *App {
	t.Helper()
	app := New(Options{Host: UnsupportedHost{}})
	app.smsHistoryPath = path
	return app
}

func at(minutes int) time.Time {
	return time.Date(2026, 8, 26, 10, minutes, 0, 0, time.UTC)
}

func TestSMSSurvivesTheProcessThatReceivedIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sms.jsonl")

	first := historyApp(t, path)
	first.RecordSMS("10086", "余额不足", at(1))
	first.RecordSMS("+447400123456", "Your code is 482913.", at(2))

	restarted := historyApp(t, path)
	messages := restarted.ListSMS()

	if len(messages) != 2 {
		t.Fatalf("重启后读回 %d 条，想要 2 条", len(messages))
	}
	// Newest first, matching what the list already promises.
	if messages[0].Sender != "+447400123456" {
		t.Errorf("messages[0].Sender = %q，顺序应为最新在前", messages[0].Sender)
	}
	if messages[1].Content != "余额不足" {
		t.Errorf("messages[1].Content = %q", messages[1].Content)
	}
	// The code is extracted on ingest, so it has to survive the round trip
	// rather than be re-derived differently on load.
	if messages[0].Code != "482913" {
		t.Errorf("messages[0].Code = %q，想要 482913", messages[0].Code)
	}
}

func TestARepeatedMessageIsStoredOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sms.jsonl")

	app := historyApp(t, path)
	app.RecordSMS("10086", "余额不足", at(1))
	app.RecordSMS("10086", "余额不足", at(1))

	// Polling re-reads what it has already seen, so a growing file would be a
	// leak rather than a cosmetic issue.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读历史文件失败: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; lines != 1 {
		t.Fatalf("文件里有 %d 行，重复消息应只落盘一次", lines)
	}
	if got := len(historyApp(t, path).ListSMS()); got != 1 {
		t.Errorf("读回 %d 条，想要 1 条", got)
	}
}

// Loading is a read, not a receipt. Folding the file back in as if it were new
// traffic would append it again, doubling the archive on every launch — and the
// list would look right the whole time, because dedup hides it in memory.
func TestRestartingDoesNotRewriteTheHistoryItJustRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sms.jsonl")

	first := historyApp(t, path)
	first.RecordSMS("10086", "第一条", at(1))
	first.RecordSMS("10086", "第二条", at(2))

	lines := func() int {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读历史文件失败: %v", err)
		}
		return strings.Count(strings.TrimSpace(string(data)), "\n") + 1
	}
	before := lines()

	for range 3 {
		historyApp(t, path).ListSMS()
	}

	if after := lines(); after != before {
		t.Fatalf("三次重启后文件从 %d 行涨到 %d 行", before, after)
	}
}

// An append can be cut short by a crash or a full disk. Refusing to read the
// file at all would turn one damaged line into total loss, which is the exact
// failure this feature exists to prevent.
func TestATruncatedFinalLineDoesNotLoseTheRestOfTheHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sms.jsonl")

	app := historyApp(t, path)
	app.RecordSMS("10086", "第一条", at(1))
	app.RecordSMS("10086", "第二条", at(2))

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读历史文件失败: %v", err)
	}
	if err := os.WriteFile(path, append(data, []byte(`{"sender":"10086","cont`)...), 0o600); err != nil {
		t.Fatalf("写入截断行失败: %v", err)
	}

	messages := historyApp(t, path).ListSMS()

	if len(messages) != 2 {
		t.Fatalf("读回 %d 条，想要 2 条完好记录", len(messages))
	}
}

// Demo mode exists so the UI can be exercised without hardware; writing its
// fixtures into the history would put invented messages in a real archive.
func TestDemoModeLeavesNoHistoryBehind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sms.jsonl")

	app := NewDemo(UnsupportedHost{})
	app.smsHistoryPath = path
	app.RecordSMS("10086", "演示消息", at(1))

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("演示模式写出了历史文件（err=%v）", err)
	}
}
