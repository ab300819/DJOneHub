package core

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// Received messages, one JSON object per line, appended in arrival order.
//
// The module deletes its copy once it has been read and the core dies with the
// app that spawned it, so this file is the only lasting record. That is what
// picks the format: an append never rewrites what is already on disk, so a
// crash can damage at most the line being written. A whole-file rewrite would
// put every message at risk on every save.
//
// Reading is deliberately forgiving. Refusing the file over one damaged line
// would turn a partial write into total loss — the very failure this exists to
// prevent.

func (a *App) smsHistoryFile() (string, error) {
	if a.smsHistoryPath != "" {
		return a.smsHistoryPath, nil
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate SMS history directory: %w", err)
	}
	a.smsHistoryPath = filepath.Join(configDir, "DJOneHub", "sms-history.jsonl")
	return a.smsHistoryPath, nil
}

// loadSMSHistoryLocked fills the in-memory list from disk on first use. Callers
// hold smsMu for writing.
func (a *App) loadSMSHistoryLocked() {
	if a.smsHistoryLoaded || a.demo {
		return
	}
	a.smsHistoryLoaded = true

	path, err := a.smsHistoryFile()
	if err != nil {
		log.Printf("SMS history unavailable: %v", err)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("read SMS history: %v", err)
		}
		return
	}
	defer func() { _ = file.Close() }()

	stored := make([]receivedSMS, 0)
	damaged := 0
	scanner := bufio.NewScanner(file)
	// A message can carry a long UCS-2 body; the default limit would stop the
	// scan partway and silently shorten the history.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var item receivedSMS
		if err := json.Unmarshal(line, &item); err != nil {
			damaged++
			continue
		}
		stored = append(stored, item)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("read SMS history: %v", err)
	}
	if damaged > 0 {
		log.Printf("SMS history: skipped %d unreadable line(s)", damaged)
	}
	a.mergeSMSLocked(stored, false)
}

// appendSMSHistory adds messages that were not already on disk. Callers hold
// smsMu for writing.
func (a *App) appendSMSHistoryLocked(messages []receivedSMS) {
	if a.demo || len(messages) == 0 {
		return
	}
	path, err := a.smsHistoryFile()
	if err != nil {
		log.Printf("SMS history unavailable: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("create SMS history directory: %v", err)
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("open SMS history: %v", err)
		return
	}
	// Close is where a failed flush surfaces, so swallowing it would drop
	// messages without ever saying so.
	defer func() {
		if err := file.Close(); err != nil {
			log.Printf("close SMS history: %v", err)
		}
	}()

	for _, item := range messages {
		line, err := json.Marshal(item)
		if err != nil {
			log.Printf("encode SMS for history: %v", err)
			continue
		}
		if _, err := file.Write(append(line, '\n')); err != nil {
			// Losing the rest of the batch is better than interleaving a
			// half-written line with the next append.
			log.Printf("append SMS history: %v", err)
			return
		}
	}
}
