package main

import (
	"encoding/csv"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/ab300819/DJOneHub/internal/service"
)

func portScore(port string) int {
	name := strings.ToLower(port)
	if strings.Contains(name, "quectel") || strings.Contains(name, "dji") {
		return 100
	}
	if strings.Contains(name, "usbmodem") {
		return 80
	}
	if strings.Contains(name, "usbserial") {
		return 60
	}
	return 0
}

func allVendorSpecific(interfaces []usbInterfaceStatus) bool {
	if len(interfaces) == 0 {
		return false
	}
	for _, iface := range interfaces {
		if iface.Class != 255 {
			return false
		}
	}
	return true
}

func intPropertyOrZero(block, name string) int {
	value, _ := intProperty(block, name)
	return value
}

func intProperty(block, name string) (int, bool) {
	pattern := regexp.MustCompile(`"` + regexp.QuoteMeta(name) + `"\s*=\s*(\d+)`)
	match := pattern.FindStringSubmatch(block)
	if len(match) != 2 {
		return 0, false
	}
	value, err := strconv.Atoi(match[1])
	return value, err == nil
}

func stringProperty(block, name string) string {
	pattern := regexp.MustCompile(`"` + regexp.QuoteMeta(name) + `"\s*=\s*"([^"]*)"`)
	match := pattern.FindStringSubmatch(block)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func formatHexProperty(block, name string) string {
	value, ok := intProperty(block, name)
	if !ok {
		return ""
	}
	return fmt.Sprintf("0x%x", value)
}

func usbSpeedName(speed int) string {
	switch speed {
	case 1:
		return "low-speed"
	case 2:
		return "full-speed"
	case 3:
		return "high-speed"
	case 4:
		return "super-speed"
	default:
		if speed == 0 {
			return ""
		}
		return fmt.Sprintf("speed-%d", speed)
	}
}

func sessionTrafficFromCounters(current, baseline networkByteCounters) (rx, tx, total uint64) {
	rx = current.RX - baseline.RX
	tx = current.TX - baseline.TX
	return rx, tx, rx + tx
}

func splitIfconfigBlocks(out string) []string {
	var blocks []string
	var current []string
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if line[0] != '\t' && line[0] != ' ' && strings.Contains(line, ":") {
			if len(current) > 0 {
				blocks = append(blocks, strings.Join(current, "\n"))
			}
			current = []string{line}
			continue
		}
		if len(current) > 0 {
			current = append(current, line)
		}
	}
	if len(current) > 0 {
		blocks = append(blocks, strings.Join(current, "\n"))
	}
	return blocks
}

func classifyMacInterfaceName(name string) string {
	switch {
	case strings.HasPrefix(name, "en"):
		return "ethernet"
	case strings.HasPrefix(name, "bridge"):
		return "bridge"
	case strings.HasPrefix(name, "awdl") || strings.HasPrefix(name, "llw") || strings.HasPrefix(name, "ap"):
		return "apple-wireless"
	default:
		return "other"
	}
}

// hasUSBNetworkInterface reports whether the module has an interface of its own
// that is up. Any other interface being up says nothing about the module.
func hasUSBNetworkInterface(interfaces []macNetInterface, moduleInterface string) bool {
	return selectUSBTrafficInterface(interfaces, moduleInterface) != ""
}

// selectUSBTrafficInterface returns the module's interface, and only that.
//
// It used to fall back to "any active ethernet that is not en0", which reported
// whatever NIC happened to be up — on a machine where en0 is wired and Wi-Fi is
// en1, that meant presenting the user's Wi-Fi traffic as the module's. Reporting
// nothing is the correct answer when the module has no interface.
func selectUSBTrafficInterface(interfaces []macNetInterface, moduleInterface string) string {
	if moduleInterface == "" {
		return ""
	}
	for _, item := range interfaces {
		if item.Name == moduleInterface && item.Status == "active" {
			return item.Name
		}
	}
	return ""
}

// nettopProcessSuffix strips the pid that nettop appends to a process name.
var nettopProcessSuffix = regexp.MustCompile(`\.\d+$`)

// parseNettopActivity reads `nettop -x` CSV. Rows without a flow name a process
// and apply to the flow rows that follow, so the current process is carried
// forward across iterations.
func parseNettopActivity(out string) []service.ActivityRecord {
	reader := csv.NewReader(strings.NewReader(out))
	reader.FieldsPerRecord = -1
	var records []service.ActivityRecord
	process := "系统"
	for {
		row, err := reader.Read()
		if err != nil {
			break
		}
		if len(row) < 6 || row[1] == "" {
			continue
		}
		description := strings.TrimSpace(row[1])
		iface := strings.TrimSpace(row[2])
		if !strings.Contains(description, "<->") {
			process = nettopProcessSuffix.ReplaceAllString(description, "")
			continue
		}
		protocol, host, port := parseNettopFlow(description)
		if host == "" || iface == "" || host == "127.0.0.1" || host == "::1" {
			continue
		}
		rx, _ := strconv.ParseUint(strings.TrimSpace(row[4]), 10, 64)
		tx, _ := strconv.ParseUint(strings.TrimSpace(row[5]), 10, 64)
		state := ""
		if strings.HasPrefix(protocol, "tcp") && len(row) > 3 {
			state = strings.TrimSpace(row[3])
		}
		record := service.ActivityRecord{
			Process: process, Port: port, Protocol: protocol,
			Interface: iface, State: state, RXBytes: rx, TXBytes: tx,
		}
		if net.ParseIP(host) == nil {
			record.Host = host
		} else {
			record.IP = host
		}
		records = append(records, record)
	}
	return records
}

// parseNettopFlow splits a "proto local<->remote" description, returning the
// remote end. IPv6 addresses carry colons of their own, so the port is split on
// the last dot for those and on the last colon otherwise.
func parseNettopFlow(description string) (protocol, host, port string) {
	fields := strings.Fields(description)
	if len(fields) < 2 {
		return "", "", ""
	}
	protocol = fields[0]
	flow := fields[len(fields)-1]
	parts := strings.SplitN(flow, "<->", 2)
	if len(parts) != 2 {
		return protocol, "", ""
	}
	remote := parts[1]
	if strings.Count(remote, ":") > 1 {
		if index := strings.LastIndex(remote, "."); index > 0 {
			return protocol, strings.Trim(remote[:index], "[]"), remote[index+1:]
		}
		return protocol, strings.Trim(remote, "[]"), ""
	}
	if index := strings.LastIndex(remote, ":"); index > 0 {
		return protocol, strings.Trim(remote[:index], "[]"), remote[index+1:]
	}
	return protocol, strings.Trim(remote, "[]"), ""
}

func parseMacInterfaceCounters(out string) map[string]networkByteCounters {
	counters := make(map[string]networkByteCounters)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || !strings.HasPrefix(fields[2], "<Link#") {
			continue
		}
		name := strings.TrimSuffix(fields[0], "*")
		rx, rxErr := strconv.ParseUint(fields[6], 10, 64)
		tx, txErr := strconv.ParseUint(fields[9], 10, 64)
		if name == "" || rxErr != nil || txErr != nil {
			continue
		}
		counters[name] = networkByteCounters{RX: rx, TX: tx}
	}
	return counters
}

func parseModuleNetworkInterface(out string) string {
	for _, block := range strings.Split(out, "\n\n") {
		vendorID, ok := intProperty(block, "idVendor")
		if !ok || (vendorID != 0x2ca3 && vendorID != 0x2c7c) {
			continue
		}
		if name := stringProperty(block, "BSD Name"); strings.HasPrefix(name, "en") {
			return name
		}
	}
	return ""
}
