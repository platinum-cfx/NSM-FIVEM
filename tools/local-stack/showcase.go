// Showcase servers — founder-curated, whitelist-only (private) display
// listings shown on the website server list and the in-game list with their
// own logos and player numbers. They are display entries: no real game server
// stands behind their endpoints (TEST-NET-3 documentation IPs, never
// routable), so they are marked private and the UI disables Connect for them.
// Their player counts always sum to exactly the platform showcase baseline
// (15,000) and the live player counter starts from there and grows with the
// real servers. All names, logos and descriptions are original content.
package main

import (
	"encoding/base64"
	"fmt"
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
	Tags       string
	Icon       string // file name under site/static/showcase/
}

// The original three flagship listings (counts 650/300/150 — the first 1.1k
// baseline; NoPixel V holds the largest share of the whole set).
var showcaseBase = []showcaseServer{
	{Ep: "203.0.113.10:30120", Hostname: "NoPixel V - Whitelisted", Desc: "Whitelisted GTA roleplay. Applications only.", Clients: 650, MaxClients: 700, Upvotes: 24820, Tags: "whitelisted,roleplay,rp", Icon: "nopixel-v.png"},
	{Ep: "203.0.113.20:30120", Hostname: "Eclipse RP - Serious Roleplay", Desc: "Serious roleplay. Whitelisted community.", Clients: 300, MaxClients: 350, Upvotes: 9140, Tags: "whitelisted,roleplay,rp", Icon: "eclipse-rp.png"},
	{Ep: "203.0.113.30:30120", Hostname: "Harborline RP", Desc: "Whitelisted coastal city roleplay.", Clients: 150, MaxClients: 200, Upvotes: 4380, Tags: "whitelisted,roleplay,rp", Icon: "harborline-rp.png"},
}

// showcaseExtra is the wider invented community set (50 listings with
// generated logos and original one-line descriptions). Their counts sum to
// 13,900 so showcaseBase + showcaseExtra = exactly 15,000.
var showcaseExtra = []showcaseServer{
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 100), Hostname: "Vantage Bay RP", Desc: "Harbor city roleplay with a living economy and whitelisted crews.", Clients: 620, MaxClients: 710, Upvotes: 5700, Tags: "roleplay,whitelisted", Icon: fmt.Sprintf("x%02d.png", 0)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 101), Hostname: "Ironside RP", Desc: "Serious story-driven roleplay. Applications open every weekend.", Clients: 598, MaxClients: 685, Upvotes: 6137, Tags: "seriousrp,whitelisted", Icon: fmt.Sprintf("x%02d.png", 1)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 102), Hostname: "Midnight Motorworks", Desc: "Underground racing, tuning culture and midnight meets.", Clients: 575, MaxClients: 660, Upvotes: 6519, Tags: "racing,drift", Icon: fmt.Sprintf("x%02d.png", 2)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 103), Hostname: "Bluffsided RP", Desc: "Small-town stories on the Bluffs. Fresh wipe, fresh start.", Clients: 552, MaxClients: 630, Upvotes: 6855, Tags: "roleplay,freshwipe", Icon: fmt.Sprintf("x%02d.png", 3)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 104), Hostname: "Apex State RP", Desc: "Balanced civilian and criminal RP with a focus on fairness.", Clients: 530, MaxClients: 605, Upvotes: 7158, Tags: "roleplay,esx", Icon: fmt.Sprintf("x%02d.png", 4)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 105), Hostname: "Copperline County", Desc: "Sheriff, fire and EMS focused county roleplay.", Clients: 508, MaxClients: 580, Upvotes: 7417, Tags: "seriousrp,leo", Icon: fmt.Sprintf("x%02d.png", 5)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 106), Hostname: "Static City RP", Desc: "Fast-paced city life with heists, jobs and street racing.", Clients: 486, MaxClients: 555, Upvotes: 7632, Tags: "roleplay,action", Icon: fmt.Sprintf("x%02d.png", 6)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 107), Hostname: "Velvet District", Desc: "Premium nightlife, fashion and business roleplay.", Clients: 465, MaxClients: 530, Upvotes: 4564, Tags: "roleplay,lifestyle", Icon: fmt.Sprintf("x%02d.png", 7)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 108), Hostname: "Kingsway RP", Desc: "Dynasties, cartels and courtrooms. Your story matters here.", Clients: 476, MaxClients: 545, Upvotes: 5176, Tags: "roleplay,whitelisted", Icon: fmt.Sprintf("x%02d.png", 8)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 109), Hostname: "Ember Coast RP", Desc: "Sunset coast vibes with surfing, fishing and beach crews.", Clients: 456, MaxClients: 520, Upvotes: 5469, Tags: "roleplay,casual", Icon: fmt.Sprintf("x%02d.png", 9)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 110), Hostname: "Northgate Stories", Desc: "Character-first RP where every arc gets a spotlight.", Clients: 437, MaxClients: 500, Upvotes: 5734, Tags: "seriousrp,story", Icon: fmt.Sprintf("x%02d.png", 10)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 111), Hostname: "Redline Syndicate", Desc: "Street racing syndicate with territory wars and pink slips.", Clients: 419, MaxClients: 480, Upvotes: 5974, Tags: "racing,pvp", Icon: fmt.Sprintf("x%02d.png", 11)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 112), Hostname: "Silverpine RP", Desc: "Mountain town roleplay with hunting, mining and survival.", Clients: 402, MaxClients: 460, Upvotes: 6192, Tags: "roleplay,survival", Icon: fmt.Sprintf("x%02d.png", 12)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 113), Hostname: "Docklands Life", Desc: "Working port city — unions, smuggling and dockside drama.", Clients: 385, MaxClients: 440, Upvotes: 6376, Tags: "roleplay,whitelisted", Icon: fmt.Sprintf("x%02d.png", 13)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 114), Hostname: "Ravenwood RP", Desc: "Gothic noir roleplay set in a rain-soaked downtown.", Clients: 369, MaxClients: 420, Upvotes: 3959, Tags: "seriousrp,noir", Icon: fmt.Sprintf("x%02d.png", 14)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 115), Hostname: "Summit Ridge RP", Desc: "Alpine county with ski patrol, tourism and mountain crews.", Clients: 354, MaxClients: 405, Upvotes: 4215, Tags: "roleplay,casual", Icon: fmt.Sprintf("x%02d.png", 15)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 116), Hostname: "Glassway RP", Desc: "Corporate city where business empires are built and broken.", Clients: 339, MaxClients: 389, Upvotes: 4441, Tags: "roleplay,business", Icon: fmt.Sprintf("x%02d.png", 16)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 117), Hostname: "Outback Trails", Desc: "Desert trucking, rally and off-road adventure servers.", Clients: 325, MaxClients: 375, Upvotes: 4649, Tags: "freeroam,racing", Icon: fmt.Sprintf("x%02d.png", 17)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 118), Hostname: "Neon Pulse RP", Desc: "Cyber-flavoured city RP with clubs, hacking and synthwave.", Clients: 312, MaxClients: 362, Upvotes: 4842, Tags: "roleplay,theme", Icon: fmt.Sprintf("x%02d.png", 18)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 119), Hostname: "Bayfront Tales", Desc: "Wholesome community RP — families, shops and local events.", Clients: 299, MaxClients: 349, Upvotes: 5009, Tags: "roleplay,community", Icon: fmt.Sprintf("x%02d.png", 19)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 120), Hostname: "Ashfield RP", Desc: "Post-industrial town rebuilding itself. Union vs mafia arcs.", Clients: 287, MaxClients: 337, Upvotes: 5165, Tags: "roleplay,whitelisted", Icon: fmt.Sprintf("x%02d.png", 20)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 121), Hostname: "Crosswind RP", Desc: "Airport and cargo hub roleplay with global trade routes.", Clients: 275, MaxClients: 325, Upvotes: 3372, Tags: "roleplay,business", Icon: fmt.Sprintf("x%02d.png", 21)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 122), Hostname: "Lumen City", Desc: "Bright and busy metropolis — always something happening.", Clients: 264, MaxClients: 314, Upvotes: 3574, Tags: "roleplay,casual", Icon: fmt.Sprintf("x%02d.png", 22)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 123), Hostname: "Old Mill County", Desc: "Historic county roleplay with farming and railroads.", Clients: 253, MaxClients: 303, Upvotes: 3754, Tags: "roleplay,farming", Icon: fmt.Sprintf("x%02d.png", 23)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 124), Hostname: "Stormchaser RP", Desc: "Extreme weather events reshape the map every season.", Clients: 243, MaxClients: 293, Upvotes: 3924, Tags: "roleplay,events", Icon: fmt.Sprintf("x%02d.png", 24)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 125), Hostname: "Palisade RP", Desc: "Cliffside luxury living, yachts and high-society drama.", Clients: 233, MaxClients: 283, Upvotes: 3174, Tags: "roleplay,lifestyle", Icon: fmt.Sprintf("x%02d.png", 25)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 126), Hostname: "Wreckers Freeroam", Desc: "Pure freeroam chaos — stunts, demolition and mayhem.", Clients: 224, MaxClients: 274, Upvotes: 3318, Tags: "freeroam,stunts", Icon: fmt.Sprintf("x%02d.png", 26)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 127), Hostname: "Nightshift RP", Desc: "The city after dark — taxi, hospital and nightlife crews.", Clients: 214, MaxClients: 264, Upvotes: 3429, Tags: "roleplay,casual", Icon: fmt.Sprintf("x%02d.png", 27)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 128), Hostname: "Highwater RP", Desc: "Flooded bayou survival with bootlegging and airboats.", Clients: 205, MaxClients: 255, Upvotes: 2101, Tags: "roleplay,survival", Icon: fmt.Sprintf("x%02d.png", 28)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 129), Hostname: "Cornerstone RP", Desc: "New-player friendly city with mentors and guided starts.", Clients: 197, MaxClients: 247, Upvotes: 2263, Tags: "roleplay,beginner", Icon: fmt.Sprintf("x%02d.png", 29)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 130), Hostname: "Driftveil City", Desc: "Dedicated drift leagues, time attack and car shows.", Clients: 189, MaxClients: 239, Upvotes: 2409, Tags: "racing,drift", Icon: fmt.Sprintf("x%02d.png", 30)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 131), Hostname: "Anchorpoint RP", Desc: "Naval town with coast guard, fishing and island hops.", Clients: 182, MaxClients: 232, Upvotes: 2551, Tags: "roleplay,leo", Icon: fmt.Sprintf("x%02d.png", 31)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 132), Hostname: "Meridian State", Desc: "All-in-one state RP — three cities, one connected map.", Clients: 175, MaxClients: 225, Upvotes: 2679, Tags: "roleplay,large", Icon: fmt.Sprintf("x%02d.png", 32)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 133), Hostname: "Backwoods RP", Desc: "Off-grid living, hunting lodges and moonshine runs.", Clients: 168, MaxClients: 218, Upvotes: 2793, Tags: "roleplay,survival", Icon: fmt.Sprintf("x%02d.png", 33)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 134), Hostname: "Frostline RP", Desc: "Winter survival season with blizzards and trade convoys.", Clients: 161, MaxClients: 211, Upvotes: 2893, Tags: "roleplay,survival", Icon: fmt.Sprintf("x%02d.png", 34)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 135), Hostname: "Speedway Legends", Desc: "Stock car, drag and open-wheel league racing.", Clients: 155, MaxClients: 205, Upvotes: 1910, Tags: "racing,league", Icon: fmt.Sprintf("x%02d.png", 35)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 136), Hostname: "Tidewater RP", Desc: "Coastal villages, lighthouses and a close-knit community.", Clients: 149, MaxClients: 199, Upvotes: 2042, Tags: "roleplay,community", Icon: fmt.Sprintf("x%02d.png", 36)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 137), Hostname: "Uptown Vault RP", Desc: "High-stakes heists and a player-run banking system.", Clients: 143, MaxClients: 193, Upvotes: 2162, Tags: "roleplay,heists", Icon: fmt.Sprintf("x%02d.png", 37)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 138), Hostname: "Wanderlust RP", Desc: "Road-trip RP — campers, motels and roadside diners.", Clients: 137, MaxClients: 187, Upvotes: 2270, Tags: "roleplay,casual", Icon: fmt.Sprintf("x%02d.png", 38)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 139), Hostname: "Beacon Hills RP", Desc: "Suburban hills with schools, sports and neighbourhood arcs.", Clients: 132, MaxClients: 182, Upvotes: 2379, Tags: "roleplay,family", Icon: fmt.Sprintf("x%02d.png", 39)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 140), Hostname: "Junkyard Kings", Desc: "Scrapyard economy — rebuild wrecks into street machines.", Clients: 127, MaxClients: 177, Upvotes: 2478, Tags: "freeroam,cars", Icon: fmt.Sprintf("x%02d.png", 40)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 141), Hostname: "Maplewood Stories", Desc: "Cozy small-town RP with seasonal festivals.", Clients: 122, MaxClients: 172, Upvotes: 2567, Tags: "roleplay,cozy", Icon: fmt.Sprintf("x%02d.png", 41)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 142), Hostname: "Siren Bay RP", Desc: "Emergency services training ground with realistic protocols.", Clients: 117, MaxClients: 167, Upvotes: 1827, Tags: "seriousrp,ems", Icon: fmt.Sprintf("x%02d.png", 42)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 143), Hostname: "Greypoint RP", Desc: "Gritty industrial city with union strikes and crime waves.", Clients: 112, MaxClients: 162, Upvotes: 1931, Tags: "roleplay,whitelisted", Icon: fmt.Sprintf("x%02d.png", 43)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 144), Hostname: "Overpass RP", Desc: "Highway trucking empire — logistics, convoys and tolls.", Clients: 107, MaxClients: 157, Upvotes: 2025, Tags: "roleplay,trucking", Icon: fmt.Sprintf("x%02d.png", 44)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 145), Hostname: "Subzero Survival", Desc: "Hardcore winter survival with permadeath seasons.", Clients: 102, MaxClients: 152, Upvotes: 2109, Tags: "survival,hardcore", Icon: fmt.Sprintf("x%02d.png", 45)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 146), Hostname: "Skylane Racing", Desc: "Air racing and stunt flying league above the city.", Clients: 97, MaxClients: 147, Upvotes: 2183, Tags: "racing,planes", Icon: fmt.Sprintf("x%02d.png", 46)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 147), Hostname: "Bricktown RP", Desc: "Historic downtown revival with artisan shops and markets.", Clients: 87, MaxClients: 137, Upvotes: 2177, Tags: "roleplay,community", Icon: fmt.Sprintf("x%02d.png", 47)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 148), Hostname: "Paradise Palms RP", Desc: "Resort town living — golf, casinos and poolside drama.", Clients: 75, MaxClients: 125, Upvotes: 2121, Tags: "roleplay,lifestyle", Icon: fmt.Sprintf("x%02d.png", 48)},
	{Ep: fmt.Sprintf("203.0.113.%d:30120", 149), Hostname: "Lakeshore RP", Desc: "Quiet lakeside retreat for relaxed slow-burn roleplay.", Clients: 61, MaxClients: 111, Upvotes: 682, Tags: "roleplay,chill", Icon: fmt.Sprintf("x%02d.png", 49)},
}

var showcaseList = append(append([]showcaseServer{}, showcaseBase...), showcaseExtra...)

func showcaseIconB64(s showcaseServer) string {
	raw, err := siteFS.ReadFile("site/static/showcase/" + s.Icon)
	if err != nil {
		log.Printf("showcase icon %s: %v", s.Icon, err)
		return ""
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// Showcase player-count noise: the numbers wander a little so the listings
// look alive with people joining and leaving, but the counts always sum to
// exactly the showcase baseline (15,000) so the platform's live player
// counter stays honest — it reads 15k + real players, never more or less.
var (
	jitterMu   sync.Mutex
	jitterWalk = map[string]int{} // endPoint -> current offset from the base count
)

func showcaseBand(s showcaseServer) int {
	band := int(s.Clients) / 20 // ±5%
	if band < 3 {
		band = 3
	}
	return band
}

// showcaseJitterStep nudges every listing inside its band, then walks the
// leftover drift off across the set so the grand total stays exact. The
// residual loop always terminates: whenever the residual is non-zero, some
// listing has slack in the needed direction.
func showcaseJitterStep() {
	jitterMu.Lock()
	defer jitterMu.Unlock()
	residual := 0
	for _, s := range showcaseList {
		band := showcaseBand(s)
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
		residual += off
	}
	for i := 0; residual != 0; i++ {
		s := showcaseList[i%len(showcaseList)]
		band := showcaseBand(s)
		off := jitterWalk[s.Ep]
		if residual > 0 && off > -band {
			jitterWalk[s.Ep] = off - 1
			residual--
		} else if residual < 0 && off < band {
			jitterWalk[s.Ep] = off + 1
			residual++
		}
	}
}

// showcaseCounts returns the current player counts (bases plus jitter); the
// sum is exactly the baseline on every call.
func showcaseCounts() []int64 {
	jitterMu.Lock()
	defer jitterMu.Unlock()
	out := make([]int64, len(showcaseList))
	for i, s := range showcaseList {
		out[i] = s.Clients + int64(jitterWalk[s.Ep])
	}
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
				"tags":           s.Tags,
				"nsm_private":    "1",
				"gamename":       gamename, // the NSM.exe browser filter requires this
				"sv_projectDesc": s.Desc,
				"locale":         "en-US",
				"country":        "US",
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
