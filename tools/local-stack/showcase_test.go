package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// TestShowcaseServers covers the founder-curated private showcase set: all
// listings appear on both list surfaces, player counts sum to exactly the
// 15k baseline with NoPixel V holding the largest share, every entry is
// private (no Connect), descriptions and gamename flow to the client list,
// and the counts wiggle while staying sum-locked.
func TestShowcaseServers(t *testing.T) {
	h := setupTest(t)
	upsertShowcase()

	// 1. The shared list API (website + NSM.exe) carries the whole set.
	w := request(h, "GET", "/api/servers/live", "")
	var live struct {
		Servers []map[string]interface{} `json:"servers"`
		Players float64                  `json:"players"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &live); err != nil {
		t.Fatalf("live JSON: %v: %s", err, w.Body.String())
	}
	if len(live.Servers) != 53 {
		t.Fatalf("live servers = %d, want 53", len(live.Servers))
	}
	byName := map[string]map[string]interface{}{}
	for _, s := range live.Servers {
		if name, _ := s["CleanName"].(string); name != "" {
			byName[name] = s
		}
	}
	flagships := []string{"NoPixel V - Whitelisted", "Eclipse RP - Serious Roleplay", "Harborline RP"}
	for _, n := range flagships {
		if _, ok := byName[n]; !ok {
			t.Fatalf("flagship server %q missing from /api/servers/live", n)
		}
	}

	// 2. Counts: sum exactly 15,000, NoPixel V is the largest, all private.
	sum := 0.0
	np := 0.0
	for _, s := range live.Servers {
		c, _ := s["Clients"].(float64)
		sum += c
		if name, _ := s["CleanName"].(string); name == "NoPixel V - Whitelisted" {
			np = c
		}
		if priv, _ := s["Private"].(bool); !priv {
			t.Fatalf("server %v not marked private", s["CleanName"])
		}
	}
	if int(sum) != 15000 {
		t.Fatalf("showcase player sum = %v, want 15000", sum)
	}
	for _, s := range live.Servers {
		c, _ := s["Clients"].(float64)
		if c > np {
			t.Fatalf("NoPixel V must have the largest share, but %v has %v", s["CleanName"], c)
		}
	}
	if int(live.Players) != 15000 {
		t.Fatalf("live players total = %v, want 15000 (15k baseline)", live.Players)
	}

	// 2b. The NSM.exe browser filter requires vars.gamename; its list also
	// shows descriptions from vars.sv_projectDesc — both must flow.
	for _, ep := range []string{"203.0.113.10:30120", "203.0.113.100:30120", "203.0.113.149:30120"} {
		ws := request(h, "GET", "/api/servers/single/"+ep, "")
		body := ws.Body.String()
		if !strings.Contains(body, `"gamename":"gta5"`) {
			t.Fatalf("showcase entry %s missing vars.gamename=gta5 (NSM.exe would hide it): %s", ep, body)
		}
		if !strings.Contains(body, `"sv_projectDesc"`) {
			t.Fatalf("showcase entry %s missing vars.sv_projectDesc (no description on the NSM.exe list): %s", ep, body)
		}
	}

	// 3. Website list: flagship names, descriptions, 15k counter, private buttons.
	page := request(h, "GET", "/servers", "").Body.String()
	for _, want := range append(flagships, "15k", "WHITELISTED", "Vantage Bay RP", "Lakeshore RP") {
		if !strings.Contains(page, want) {
			t.Fatalf("/servers missing %q", want)
		}
	}
	if got := strings.Count(page, "data-sl-private"); got != 53 {
		t.Fatalf("/servers private buttons = %d, want 53", got)
	}
	if strings.Contains(page, `data-sl-connect="203.0.113.`) {
		t.Fatalf("/servers must not offer Connect for private showcase servers")
	}
	for _, ep := range []string{"203.0.113.10:30120", "203.0.113.100:30120"} {
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

	// 5. Homepage counter reads 15k.
	home := request(h, "GET", "/", "").Body.String()
	if !strings.Contains(home, "15k") {
		t.Fatalf("homepage missing 15k player count")
	}

	// 6. Detail pages honor privacy too.
	code := joinCodeForEndpoint("203.0.113.10:30120")
	detail := request(h, "GET", "/join/"+code, "").Body.String()
	if !strings.Contains(detail, "Whitelisted") {
		t.Fatalf("private server detail page missing Whitelisted connect state")
	}

	// 7. Counts wiggle like real join/leave traffic but stay sum-locked to 15k.
	moves := 0
	last := map[string]float64{}
	for i := 0; i < 30; i++ {
		showcaseJitterStep()
		upsertShowcase()
		w2 := request(h, "GET", "/api/servers/live", "")
		var live2 struct {
			Servers []map[string]interface{} `json:"servers"`
		}
		if err := json.Unmarshal(w2.Body.Bytes(), &live2); err != nil {
			t.Fatalf("live JSON: %v", err)
		}
		sum2 := 0.0
		changed := false
		for _, s := range live2.Servers {
			name, _ := s["CleanName"].(string)
			c, _ := s["Clients"].(float64)
			sum2 += c
			if prev, ok := last[name]; ok && prev != c {
				changed = true
			}
			last[name] = c
		}
		if int(sum2) != 15000 {
			t.Fatalf("jitter broke the 15k sum: %v", sum2)
		}
		// every listing stays within its ±5% band around the base
		for _, s := range live2.Servers {
			name, _ := s["CleanName"].(string)
			c, _ := s["Clients"].(float64)
			base := 0.0
			for _, b := range append(showcaseBase, showcaseExtra...) {
				if b.Hostname == name {
					base = float64(b.Clients)
				}
			}
			band := base / 20
			if band < 3 {
				band = 3
			}
			if c < base-band || c > base+band {
				t.Fatalf("%s count %v left band (%v±%v)", name, c, base, band)
			}
		}
		if changed {
			moves++
		}
	}
	if moves == 0 {
		t.Fatalf("player counts never changed across 30 jitter steps")
	}
}
