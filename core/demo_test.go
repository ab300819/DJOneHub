package core

import (
	"encoding/json"
	"testing"
)

// Demo mode exists so the UI can be built and reviewed without hardware. A
// reading that is absent there is not a neutral gap: the app correctly treats
// "cannot read" as "the module cannot do this", so the feature disappears from
// the screen entirely and cannot be reviewed at all.
//
// @verifies AC-129, AC-130, AC-133
// @testcase IT-014

func demoResult(t *testing.T, request string) map[string]any {
	t.Helper()
	response := decodeResponse(t, Call(NewDemo(UnsupportedHost{}), []byte(request)))
	if !response.OK {
		t.Fatalf("%s failed in demo mode: %s", request, response.Error)
	}
	encoded, err := json.Marshal(response.Result)
	if err != nil {
		t.Fatalf("result cannot be re-encoded: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatalf("result is not a JSON object: %v (%s)", err, encoded)
	}
	return result
}

// Without a capacity the app cannot tell an empty phonebook from a module that
// has none, so it hides the editor — which is what made this invisible under
// `make demo`.
func TestDemoModuleNotesReportCapacity(t *testing.T) {
	result := demoResult(t, `{"id":1,"method":"esim.moduleNotes.list"}`)

	total, ok := result["total"].(float64)
	if !ok || total <= 0 {
		t.Fatalf("total = %v, want a positive capacity", result["total"])
	}
	if _, ok := result["notes"].(map[string]any); !ok {
		t.Fatalf("notes = %v, want an object even when empty", result["notes"])
	}
	used, ok := result["used"].(float64)
	if !ok || used > total {
		t.Fatalf("used = %v, want at most total (%v)", result["used"], total)
	}
}

// The notes are what the editor renders. An empty phonebook only ever shows the
// empty state, so demo mode cannot stand in for a review of the editor itself.
func TestDemoModuleNotesCarryEntries(t *testing.T) {
	result := demoResult(t, `{"id":1,"method":"esim.moduleNotes.list"}`)

	notes, _ := result["notes"].(map[string]any)
	if len(notes) == 0 {
		t.Fatal("notes is empty; demo cannot show what the editor looks like with content")
	}
	if used, _ := result["used"].(float64); int(used) != len(notes) {
		t.Errorf("used = %v but %d notes were returned", result["used"], len(notes))
	}
	for iccid, raw := range notes {
		note, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("note %q is not an object: %v", iccid, raw)
		}
		if note["iccid"] != iccid {
			t.Errorf("note keyed by %q carries iccid %v", iccid, note["iccid"])
		}
		if label, _ := note["label"].(string); label == "" {
			t.Errorf("note %q has no label", iccid)
		}
	}
}

// The hardware panel is driven entirely by usb_device. Absent, the whole
// section vanishes under `make demo`.
func TestDemoDiagnosticCarriesUSBDevice(t *testing.T) {
	result := demoResult(t, `{"id":1,"method":"network.diagnostic"}`)

	device, ok := result["usb_device"].(map[string]any)
	if !ok {
		t.Fatalf("usb_device = %v, want an object", result["usb_device"])
	}
	for _, field := range []string{"product", "vendor", "vendor_id", "product_id", "location_id", "speed", "mode"} {
		if value, _ := device[field].(string); value == "" {
			t.Errorf("usb_device.%s is empty", field)
		}
	}
	interfaces, ok := device["interfaces"].([]any)
	if !ok || len(interfaces) == 0 {
		t.Fatalf("usb_device.interfaces = %v, want a non-empty list", device["interfaces"])
	}
	// The interface list is what the app reads to explain the current usbnet
	// mode, so a shape that cannot express both ECM interfaces would let a
	// wrong explanation pass review.
	classes := make(map[float64]int)
	for _, raw := range interfaces {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("interface entry is not an object: %v", raw)
		}
		for _, field := range []string{"number", "class", "subclass", "protocol", "endpoints"} {
			if _, ok := entry[field].(float64); !ok {
				t.Errorf("interface %v is missing %s", entry["number"], field)
			}
		}
		class, _ := entry["class"].(float64)
		classes[class]++
	}
	for _, want := range []float64{2, 10} {
		if classes[want] == 0 {
			t.Errorf("no interface of class %v; demo cannot represent an ECM pair", want)
		}
	}
}
