// Showcase servers — founder-curated, whitelist-only (private) listings shown
// on the website server list and the in-game list with their own logos and
// player numbers. They are display entries: no real game server stands behind
// their endpoints (TEST-NET-3 documentation IPs, never routable), so they are
// marked private and the UI disables Connect for them. Their player counts add
// up to the platform's showcase baseline (1.1k) and the live player counter
// starts from there and grows with the real servers.
package main

import (
	"encoding/base64"
	"log"
	"math/rand"
	"sync"
	"time"
)

type showcaseServer struct {
	Ep         string
	Hostname   string
	Desc       string
	Clients    int64
	MaxClients int64
	Upvotes    int64
	Icon       string // file name under site/static/showcase/
}

// showcaseList is the full showcase set: exactly three servers, player counts
// sum to 1100, and NoPixel V holds the largest share.
var showcaseList = []showcaseServer{
	{Ep: "203.0.113.10:30120", Hostname: "NoPixel V - Whitelisted", Desc: "Whitelisted GTA roleplay. Applications only.", Clients: 650, MaxClients: 700, Upvotes: 24820, Icon: "nopixel-v.png"},
	{Ep: "203.0.113.20:30120", Hostname: "Eclipse RP - Serious Roleplay", Desc: "Serious roleplay. Whitelisted community.", Clients: 300, MaxClients: 350, Upvotes: 9140, Icon: "eclipse-rp.png"},
	{Ep: "203.0.113.30:30120", Hostname: "Harborline RP", Desc: "Whitelisted coastal city roleplay.", Clients: 150, MaxClients: 200, Upvotes: 4380, Icon: "harborline-rp.png"},
}

func showcaseIconB64(s showcaseServer) string {
	raw, err := siteFS.ReadFile("site/static/showcase/" + s.Icon)
	if err != nil {
		log.Printf("showcase icon %s: %v", s.Icon, err)
		return ""
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// Showcase player-count noise: the numbers wander a little so the listings look
// alive with people joining and leaving, but the three counts always sum to
// exactly the showcase baseline (1100) so the platform's live player counter
// stays honest — it reads 1.1k + real players, never more or less.
var (
	jitterMu   sync.Mutex
	jitterWalk = map[string]int{} // endPoint -> current offset from the base count
)

// showcaseJitterStep nudges every non-lead server's count inside a small band
// (±5% of its base) and leaves the lead server (NoPixel V) to absorb the
// difference, which keeps the grand total pinned.
func showcaseJitterStep() {
	jitterMu.Lock()
	defer jitterMu.Unlock()
	for i, s := range showcaseList {
		if i == 0 {
			continue
		}
		band := int(s.Clients) / 20 // ±5%
		if band < 3 {
			band = 3
		}
		step := int(s.Clients) / 100
		if step < 2 {
			step = 2
		}
		off := jitterWalk[s.Ep] + rand.Intn(2*step+1) - step
		if off < -band {
			off = -band
		}
		if off > band {
			off = band
		}
		jitterWalk[s.Ep] = off
	}
}

// showcaseCounts returns the current player counts: jittered bases for all but
// the lead server, which gets whatever remains so the sum is exact.
func showcaseCounts() []int64 {
	jitterMu.Lock()
	defer jitterMu.Unlock()
	total := int64(0)
	for _, s := range showcaseList {
		total += s.Clients
	}
	out := make([]int64, len(showcaseList))
	rest := int64(0)
	for i, s := range showcaseList {
		if i == 0 {
			continue
		}
		out[i] = s.Clients + int64(jitterWalk[s.Ep])
		rest += out[i]
	}
	out[0] = total - rest
	return out
}

// upsertShowcase registers (or refreshes) the showcase entries so they stay in
// the live registry like any heartbeating server.
func upsertShowcase() {
	counts := showcaseCounts()
	for i, s := range showcaseList {
		iconVersion := reg.setIcon(s.Ep, showcaseIconB64(s))
		reg.upsert(s.Ep, ServerData{
			SVMaxclients:        s.MaxClients,
			Clients:             counts[i],
			Hostname:            s.Hostname,
			Gametype:            "Roleplay",
			Mapname:             "Los Santos",
			Server:              "nsm-showcase",
			IconVersion:         iconVersion,
			EnhancedHostSupport: true,
			Vars: map[string]string{
				"tags":        "whitelisted,roleplay,rp",
				"nsm_private": "1",
				"gamename":    gamename, // the NSM.exe browser filter requires this
				"locale":      "en-US",
				"country":     "US",
			},
			UpvotePower: s.Upvotes,
		})
		joinCodeForEndpoint(s.Ep)
	}
}

// startShowcaseServers upserts the showcase entries now and keeps them fresh,
// nudging the player counts every 30s so they read like real join/leave flow.
// Tests call upsertShowcase/showcaseJitterStep directly instead of the ticker.
func startShowcaseServers() {
	upsertShowcase()
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			showcaseJitterStep()
			upsertShowcase()
		}
	}()
}
