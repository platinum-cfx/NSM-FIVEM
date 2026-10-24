package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// TestShowcaseServers covers the three founder-curated private showcase
// servers: they appear on both list surfaces, their player counts sum to the
// 1.1k baseline with NoPixel V holding the largest share, they are private
// (no Connect), and the counters read 1.1k.
func TestShowcaseServers(t *testing.T) {
	h := setupTest(t)
	upsertShowcase()

	// 1. The shared list API (website + NSM.exe) carries all three.
	w := request(h, "GET", "/api/servers/live", "")
	var live struct {
		Servers []map[string]interface{} `json:"servers"`
		Players float64                  `json:"players"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &live); err != nil {
		t.Fatalf("live JSON: %v: %s", err, w.Body.String())
	}
	byName := map[string]map[string]interface{}{}
	for _, s := range live.Servers {
		if name, _ := s["CleanName"].(string); name != "" {
			byName[name] = s
		}
	}
	names := []string{"NoPixel V - Whitelisted", "Eclipse RP - Serious Roleplay", "Harborline RP"}
	var seen []map[string]interface{}
	for _, n := range names {
		s, ok := byName[n]
		if !ok {
			t.Fatalf("showcase server %q missing from /api/servers/live", n)
		}
		seen = append(seen, s)
	}

	// 2. Player counts: sum exactly 1100, NoPixel V has the most.
	sum := 0.0
	for _, s := range seen {
		c, _ := s["Clients"].(float64)
		sum += c
		if priv, _ := s["Private"].(bool); !priv {
			t.Fatalf("server %v not marked private", s["CleanName"])
		}
	}
	if int(sum) != 1100 {
		t.Fatalf("showcase player sum = %v, want 1100", sum)
	}
	np := seen[0]["Clients"].(float64)
	if np <= seen[1]["Clients"].(float64) || np <= seen[2]["Clients"].(float64) {
		t.Fatalf("NoPixel V must have the largest share: %v vs %v/%v", np, seen[1]["Clients"], seen[2]["Clients"])
	}
	if int(live.Players) != 1100 {
		t.Fatalf("live players total = %v, want 1100 (baseline 1.1k)", live.Players)
	}

	// 3. Website list: names, logos, whitelist badges, disabled Connect.
	page := request(h, "GET", "/servers", "").Body.String()
	for _, want := range append(names, "1.1k", "WHITELISTED") {
		if !strings.Contains(page, want) {
			t.Fatalf("/servers missing %q", want)
		}
	}
	if got := strings.Count(page, "data-sl-private"); got != 3 {
		t.Fatalf("/servers private buttons = %d, want 3", got)
	}
	if strings.Contains(page, `data-sl-connect="203.0.113.`) {
		t.Fatalf("/servers must not offer Connect for private showcase servers")
	}
	for _, ep := range []string{"203.0.113.10:30120", "203.0.113.20:30120", "203.0.113.30:30120"} {
		if !strings.Contains(page, "/api/servers/icon/"+ep+"/") {
			t.Fatalf("/servers missing icon URL for %s", ep)
		}
	}

	// 4. Real logo bytes are served for the NoPixel V icon.
	m := regexp.MustCompile(`api/servers/icon/203\.0\.113\.10:30120/(\d+)\.png`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("/servers has no icon version URL for NoPixel V")
	}
	wIcon := request(h, "GET", "/api/servers/icon/203.0.113.10:30120/"+m[1]+".png", "")
	if wIcon.Code != 200 || !strings.HasPrefix(wIcon.Body.String()[:4], "\x89PNG") {
		t.Fatalf("icon fetch failed: code=%d", wIcon.Code)
	}

	// 5. Homepage counter reads 1.1k.
	home := request(h, "GET", "/", "").Body.String()
	if !strings.Contains(home, "1.1k") {
		t.Fatalf("homepage missing 1.1k player count")
	}

	// 6. Detail pages honor privacy too.
	code := joinCodeForEndpoint("203.0.113.10:30120")
	detail := request(h, "GET", "/join/"+code, "").Body.String()
	if !strings.Contains(detail, "Whitelisted") {
		t.Fatalf("private server detail page missing Whitelisted connect state")
	}
}
