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

// upsertShowcase registers (or refreshes) the showcase entries so they stay in
// the live registry like any heartbeating server.
func upsertShowcase() {
	for _, s := range showcaseList {
		iconVersion := reg.setIcon(s.Ep, showcaseIconB64(s))
		reg.upsert(s.Ep, ServerData{
			SVMaxclients:        s.MaxClients,
			Clients:             s.Clients,
			Hostname:            s.Hostname,
			Gametype:            "Roleplay",
			Mapname:             "Los Santos",
			Server:              "nsm-showcase",
			IconVersion:         iconVersion,
			EnhancedHostSupport: true,
			Vars: map[string]string{
				"tags":        "whitelisted,roleplay,rp",
				"nsm_private": "1",
				"locale":      "en-US",
				"country":     "US",
			},
			UpvotePower: s.Upvotes,
		})
		joinCodeForEndpoint(s.Ep)
	}
}

// startShowcaseServers upserts the showcase entries now and keeps them fresh.
// Tests call upsertShowcase directly instead of starting the ticker.
func startShowcaseServers() {
	upsertShowcase()
	go func() {
		t := time.NewTicker(2 * time.Minute)
		defer t.Stop()
		for range t.C {
			upsertShowcase()
		}
	}()
}
