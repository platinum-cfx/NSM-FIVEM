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

	// 2b. The NSM.exe browser filter requires vars.gamename — every entry must carry it.
	for _, ep := range []string{"203.0.113.10:30120", "203.0.113.20:30120", "203.0.113.30:30120"} {
		ws := request(h, "GET", "/api/servers/single/"+ep, "")
		if !strings.Contains(ws.Body.String(), `"gamename":"gta5"`) {
			t.Fatalf("showcase entry %s missing vars.gamename=gta5 (NSM.exe would hide it): %s", ep, ws.Body.String())
		}
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

	// 7. Counts wiggle like real join/leave traffic but stay sum-locked to 1.1k.
	moves := 0
	var last []float64
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
		bn := map[string]float64{}
		for _, s := range live2.Servers {
			if name, _ := s["CleanName"].(string); name != "" {
				bn[name], _ = s["Clients"].(float64)
			}
		}
		np2, ec2, hb2 := bn["NoPixel V - Whitelisted"], bn["Eclipse RP - Serious Roleplay"], bn["Harborline RP"]
		if int(np2+ec2+hb2) != 1100 {
			t.Fatalf("jitter broke the 1.1k sum: %v + %v + %v", np2, ec2, hb2)
		}
		if !(np2 > ec2 && np2 > hb2) {
			t.Fatalf("NoPixel V lost the largest share: %v/%v/%v", np2, ec2, hb2)
		}
		if np2 < 625 || np2 > 675 || ec2 < 285 || ec2 > 315 || hb2 < 143 || hb2 > 157 {
			t.Fatalf("counts left plausible bands: %v/%v/%v", np2, ec2, hb2)
		}
		if last != nil && (np2 != last[0] || ec2 != last[1] || hb2 != last[2]) {
			moves++
		}
		last = []float64{np2, ec2, hb2}
	}
	if moves == 0 {
		t.Fatalf("player counts never changed across 30 jitter steps")
	}
}
