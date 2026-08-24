package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

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

func (a *app) phonebookProbeCommand(command string, result *phonebookProbeResult) bool {
	response, err := a.runATCommand(command, 6*time.Second)
	if err != nil {
		result.Responses[command] = err.Error()
		return false
	}
	result.Responses[command] = strings.TrimSpace(response)
	return atCommandSucceeded(response)
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
