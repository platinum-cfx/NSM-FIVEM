package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestStatusPage drives the monitor against the real site handler in-process:
// a clean cycle reports all-operational on the page and JSON API, sustained
// failures open an incident, and recovery resolves it.
func TestStatusPage(t *testing.T) {
	h := setupTest(t)

	oldReq := statusDoRequest
	oldBase := statusProbeBase
	oldFile := statusFilePathOverride
	statusFilePathOverride = t.TempDir() + "/status.json"
	statusMu.Lock()
	statusMon = map[string]*statusComponent{}
	statusIncidents = nil
	statusMu.Unlock()
	defer func() {
		statusDoRequest = oldReq
		statusProbeBase = oldBase
		statusFilePathOverride = oldFile
		statusMu.Lock()
		statusMon = map[string]*statusComponent{}
		statusIncidents = nil
		statusMu.Unlock()
	}()

	// in-process probes against the full site handler
	statusDoRequest = func(path string) (int, string, error) {
		w := request(h, "GET", path, "")
		return w.Code, w.Body.String(), nil
	}

	// 1. clean cycle: everything operational
	runStatusCycle()
	w := request(h, "GET", "/api/status", "")
	var v statusView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("status JSON: %v: %s", err, w.Body.String())
	}
	if v.OverallText != "All Systems Operational" {
		t.Fatalf("overall = %q, want All Systems Operational", v.OverallText)
	}
	if len(v.Components) != 6 {
		t.Fatalf("components = %d, want 6", len(v.Components))
	}
	seen := map[string]bool{}
	for _, c := range v.Components {
		seen[c.Name] = true
		if c.Status != "operational" {
			t.Fatalf("component %s status = %s, want operational", c.ID, c.Status)
		}
		if len(c.DayBars) != 90 {
			t.Fatalf("component %s day bars = %d, want 90", c.ID, len(c.DayBars))
		}
		if c.LastChecked == 0 {
			t.Fatalf("component %s never checked", c.ID)
		}
	}
	for _, name := range []string{"Website & Dashboard", "Server List API", "NSM.exe Client Feed", "Server Ingestion", "Join Links", "Accounts & Sign-In"} {
		if !seen[name] {
			t.Fatalf("missing component %q", name)
		}
	}

	// 2. page renders the banner, systems and incidents sections
	page := request(h, "GET", "/status", "").Body.String()
	for _, want := range []string{"All Systems Operational", "Systems", "Past incidents", "NSM.exe Client Feed", "Server Ingestion", "uptime over 90 days", "No incidents"} {
		if !strings.Contains(page, want) {
			t.Fatalf("/status missing %q", want)
		}
	}

	// 3. sustained failures on auth open an incident
	statusDoRequest = func(path string) (int, string, error) {
		if path == "/login" {
			return 0, "", errors.New("connection refused")
		}
		w := request(h, "GET", path, "")
		return w.Code, w.Body.String(), nil
	}
	for i := 0; i < 3; i++ {
		runStatusCycle()
	}
	w = request(h, "GET", "/api/status", "")
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("status JSON: %v", err)
	}
	if v.OverallText != "Major System Outage" {
		t.Fatalf("overall during auth outage = %q", v.OverallText)
	}
	if len(v.Incidents) != 1 || v.Incidents[0].Resolved != 0 {
		t.Fatalf("expected one ongoing incident, got %+v", v.Incidents)
	}
	if len(v.Incidents[0].Updates) == 0 || v.Incidents[0].Updates[0].State != "investigating" {
		t.Fatalf("incident missing investigating update: %+v", v.Incidents[0])
	}
	page = request(h, "GET", "/status", "").Body.String()
	if !strings.Contains(page, "Automated monitoring detected failures") || !strings.Contains(page, "Accounts &amp; Sign-In") {
		t.Fatalf("/status missing incident text")
	}
	if !strings.Contains(page, "Ongoing") {
		t.Fatalf("/status missing Ongoing badge")
	}

	// 4. recovery resolves the incident and health returns
	statusDoRequest = func(path string) (int, string, error) {
		w := request(h, "GET", path, "")
		return w.Code, w.Body.String(), nil
	}
	for i := 0; i < 3; i++ {
		runStatusCycle()
	}
	w = request(h, "GET", "/api/status", "")
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("status JSON: %v", err)
	}
	if len(v.Incidents) != 1 || v.Incidents[0].Resolved == 0 {
		t.Fatalf("incident not resolved after recovery: %+v", v.Incidents)
	}
	lastUp := v.Incidents[0].Updates[len(v.Incidents[0].Updates)-1]
	if lastUp.State != "resolved" {
		t.Fatalf("last incident update = %+v, want resolved", lastUp)
	}
	for i := 0; i < 30; i++ {
		runStatusCycle()
	}
	w = request(h, "GET", "/api/status", "")
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("status JSON: %v", err)
	}
	if v.OverallText != "All Systems Operational" {
		t.Fatalf("overall after recovery = %q", v.OverallText)
	}
	page = request(h, "GET", "/status", "").Body.String()
	if !strings.Contains(page, "Resolved") {
		t.Fatalf("/status missing Resolved badge")
	}
}
