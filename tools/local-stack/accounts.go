// NSM accounts: users, custom profiles, sessions, client tokens, roles (owner/admin/staff/user), upvotes.
// Storage: accounts.json next to the exe. Passwords: bcrypt.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID               int64  `json:"id"`
	Username         string `json:"username"`
	DisplayName      string `json:"displayName,omitempty"`
	Title            string `json:"title,omitempty"`
	Bio              string `json:"bio,omitempty"`
	Signature        string `json:"signature,omitempty"`
	AvatarURL        string `json:"avatarUrl,omitempty"`
	AvatarData       string `json:"avatarData,omitempty"` // data:image/...;base64,...
	AvatarColor      string `json:"avatarColor,omitempty"`
	BannerColor      string `json:"bannerColor,omitempty"`
	BannerURL        string `json:"bannerUrl,omitempty"`
	BannerData       string `json:"bannerData,omitempty"` // data:image/...;base64,... (supports animated GIF banners)
	ThemePrimary     string `json:"themePrimary,omitempty"`
	ThemeAccent      string `json:"themeAccent,omitempty"`
	AvatarDecoration string `json:"avatarDecoration,omitempty"` // cat_ears, sovereign_crown, cyber_visor, plasma_flames, volt_lightning, sakura_drift, singularity_orb, none
	ProfileEffect    string `json:"profileEffect,omitempty"`    // aurora_waves, cyber_matrix, starlight_warp, thunder_storm, solar_embers, holo_prism, none
	PulseTier        string `json:"pulseTier,omitempty"`        // core, pro, sovereign, eternal
	PulseSince       int64  `json:"pulseSince,omitempty"`
	IsPartner        bool   `json:"isPartner,omitempty"`
	SquadHouse       string `json:"squadHouse,omitempty"` // bravery, brilliance, balance, all
	Location         string `json:"location,omitempty"`
	Website          string `json:"website,omitempty"`
	DiscordTag       string `json:"discordTag,omitempty"`
	GitHub           string `json:"github,omitempty"`
	Steam            string `json:"steam,omitempty"`
	YouTube          string `json:"youtube,omitempty"`
	Twitch           string `json:"twitch,omitempty"`
	Twitter          string `json:"twitter,omitempty"`
	FavoriteServer   string `json:"favoriteServer,omitempty"`
	Email            string   `json:"email"`
	GoogleID         string `json:"googleId,omitempty"`
	Pass             string   `json:"pass"`
	Role             string   `json:"role"`
	CreatedAt        int64    `json:"createdAt"`
	UpdatedAt        int64    `json:"updatedAt,omitempty"`
	LastLogin        int64    `json:"lastLogin,omitempty"`
	ProfileViews     int      `json:"profileViews,omitempty"`
	ProfileViewedBy  []string `json:"profileViewedBy,omitempty"`
	TopicsViewed     []int64  `json:"topicsViewed,omitempty"`
	PostsRead        int      `json:"postsRead,omitempty"`
	DaysVisitedCount int      `json:"daysVisitedCount,omitempty"`
	LastVisitDay     string   `json:"lastVisitDay,omitempty"`
	Friends           []string `json:"friends,omitempty"`
	FriendRequestsIn  []string `json:"friendRequestsIn,omitempty"`
	FriendRequestsOut []string `json:"friendRequestsOut,omitempty"`
	EmailVerified     bool     `json:"emailVerified,omitempty"`
	EmailVerifyCode   string   `json:"emailVerifyCode,omitempty"`
	UpvoteAllocation  int      `json:"upvoteAllocation,omitempty"` // 0-10 upvotes placed on the user's own server via Keymaster
	TwoFactorEnabled  bool     `json:"twoFactorEnabled,omitempty"`
	TwoFactorCode     string   `json:"twoFactorCode,omitempty"`
	Banned            bool     `json:"banned,omitempty"`
	BanReason         string   `json:"banReason,omitempty"`
	BannedAt          int64    `json:"bannedAt,omitempty"`
	BannedBy          string   `json:"bannedBy,omitempty"`
}

func (u *User) IsFounderAccount() bool {
	if u == nil {
		return false
	}
	return strings.EqualFold(u.Username, ownerName) || strings.EqualFold(u.Username, "nta~") || strings.EqualFold(u.Username, "Cravenz") || u.Role == "owner" || u.ID == 1
}

func (u *User) HasFriend(otherUsername string) bool {
	if u == nil {
		return false
	}
	otherUsername = strings.TrimPrefix(strings.TrimSpace(otherUsername), "@")
	if otherUsername == "" {
		return false
	}
	for _, f := range u.Friends {
		if strings.EqualFold(strings.TrimSpace(f), otherUsername) {
			return true
		}
	}
	return false
}

func (u *User) HasIncomingFriendRequest(otherUsername string) bool {
	if u == nil {
		return false
	}
	otherUsername = strings.TrimPrefix(strings.TrimSpace(otherUsername), "@")
	if otherUsername == "" {
		return false
	}
	for _, f := range u.FriendRequestsIn {
		if strings.EqualFold(strings.TrimSpace(f), otherUsername) {
			return true
		}
	}
	return false
}

func (u *User) HasOutgoingFriendRequest(otherUsername string) bool {
	if u == nil {
		return false
	}
	otherUsername = strings.TrimPrefix(strings.TrimSpace(otherUsername), "@")
	if otherUsername == "" {
		return false
	}
	for _, f := range u.FriendRequestsOut {
		if strings.EqualFold(strings.TrimSpace(f), otherUsername) {
			return true
		}
	}
	return false
}

func (u *User) IsStaffOrHigher() bool {
	if u == nil {
		return false
	}
	return u.IsFounderAccount() || u.Role == "owner" || u.Role == "admin" || u.Role == "staff"
}

func (u *User) EffectivePulseTier() string {
	if u == nil {
		return ""
	}
	if u.IsFounderAccount() {
		return "eternal"
	}
	switch strings.ToLower(strings.TrimSpace(u.PulseTier)) {
	case "eternal", "sovereign", "pro", "core":
		return strings.ToLower(strings.TrimSpace(u.PulseTier))
	default:
		return ""
	}
}

func (u *User) EffectivePulseTierLabel() string {
	switch u.EffectivePulseTier() {
	case "eternal":
		return "AFX Pulse Eternal (Founder Forever)"
	case "sovereign":
		return "AFX Pulse Sovereign"
	case "pro":
		return "AFX Pulse Pro"
	case "core":
		return "AFX Pulse Core"
	default:
		return "Standard Member"
	}
}

func (u *User) HasPulse() bool {
	return u != nil && u.EffectivePulseTier() != ""
}

func (u *User) HasPulseProOrHigher() bool {
	if u == nil {
		return false
	}
	t := u.EffectivePulseTier()
	return t == "pro" || t == "sovereign" || t == "eternal"
}

// UpvoteAllowance returns how many server upvotes a user can place:
// - NSM Founder (nta~): 1,000,000 upvotes
// - Every AFX account: 10 free upvotes (paid packs land later)
func (u *User) UpvoteAllowance() int {
	if u == nil {
		return 0
	}
	if u.IsFounderAccount() {
		return 1000000
	}
	return 10
}

func (u *User) HasPartnerBadge() bool {
	if u == nil {
		return false
	}
	return u.IsPartner || u.Role == "partner" || u.IsFounderAccount()
}

func (u *User) EffectiveAvatarDecoration() string {
	if u == nil || !u.HasPulse() {
		return "none"
	}
	d := strings.ToLower(strings.TrimSpace(u.AvatarDecoration))
	switch d {
	case "cat_ears", "sovereign_crown", "cyber_visor", "plasma_flames", "volt_lightning", "sakura_drift", "singularity_orb", "none":
		return d
	}
	if u.IsFounderAccount() {
		return "cat_ears"
	}
	return "none"
}

func (u *User) EffectiveProfileEffect() string {
	if u == nil || !u.HasPulseProOrHigher() {
		return "none"
	}
	e := strings.ToLower(strings.TrimSpace(u.ProfileEffect))
	switch e {
	case "aurora_waves", "cyber_matrix", "starlight_warp", "thunder_storm", "solar_embers", "holo_prism", "none":
		return e
	}
	if u.IsFounderAccount() {
		return "aurora_waves"
	}
	return "none"
}

func (u *User) GitHubHandle() string {
	if u == nil {
		return ""
	}
	g := strings.TrimSpace(u.GitHub)
	g = strings.TrimPrefix(g, "https://github.com/")
	g = strings.TrimPrefix(g, "http://github.com/")
	g = strings.TrimPrefix(g, "github.com/")
	g = strings.TrimPrefix(g, "@")
	return strings.TrimSuffix(g, "/")
}

func (u *User) GitHubURL() string {
	if u == nil || strings.TrimSpace(u.GitHub) == "" {
		return ""
	}
	g := strings.TrimSpace(u.GitHub)
	if strings.HasPrefix(g, "http://") || strings.HasPrefix(g, "https://") {
		return g
	}
	return "https://github.com/" + u.GitHubHandle()
}

func (u *User) SteamURL() string {
	if u == nil || strings.TrimSpace(u.Steam) == "" {
		return ""
	}
	s := strings.TrimSpace(u.Steam)
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	return "https://steamcommunity.com/id/" + url.PathEscape(s)
}

func (u *User) YouTubeURL() string {
	if u == nil || strings.TrimSpace(u.YouTube) == "" {
		return ""
	}
	y := strings.TrimSpace(u.YouTube)
	if strings.HasPrefix(y, "http://") || strings.HasPrefix(y, "https://") {
		return y
	}
	return "https://youtube.com/@" + strings.TrimPrefix(y, "@")
}

func (u *User) TwitchURL() string {
	if u == nil || strings.TrimSpace(u.Twitch) == "" {
		return ""
	}
	t := strings.TrimSpace(u.Twitch)
	if strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://") {
		return t
	}
	return "https://twitch.tv/" + strings.TrimPrefix(t, "@")
}

func (u *User) TwitterURL() string {
	if u == nil || strings.TrimSpace(u.Twitter) == "" {
		return ""
	}
	t := strings.TrimSpace(u.Twitter)
	if strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://") {
		return t
	}
	return "https://x.com/" + strings.TrimPrefix(t, "@")
}

func (u *User) EffectiveDisplayName() string {
	if u == nil {
		return ""
	}
	if strings.TrimSpace(u.DisplayName) != "" {
		return u.DisplayName
	}
	return u.Username
}

func (u *User) EffectiveTitle() string {
	if u == nil {
		return ""
	}
	if strings.TrimSpace(u.Title) != "" {
		return u.Title
	}
	switch u.Role {
	case "owner":
		return "NSM Founder & Owner"
	case "admin":
		return "NSM Administrator"
	case "staff":
		return "NSM Staff"
	case "partner":
		return "AFX Partnered Server Owner"
	default:
		if u.IsPartner {
			return "AFX Partnered Server Owner"
		}
		return "NSM Community Member"
	}
}

func isValidUsername(s string) bool {
	if len(s) < 3 || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.' || c == '~') {
			return false
		}
	}
	return true
}

func isValidHexColor(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for i := 1; i < 7; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func hexToRGBString(hexStr string, defR, defG, defB int) string {
	defaultStr := strconv.Itoa(defR) + ", " + strconv.Itoa(defG) + ", " + strconv.Itoa(defB)
	if !isValidHexColor(hexStr) {
		return defaultStr
	}
	r, err1 := strconv.ParseInt(hexStr[1:3], 16, 32)
	g, err2 := strconv.ParseInt(hexStr[3:5], 16, 32)
	b, err3 := strconv.ParseInt(hexStr[5:7], 16, 32)
	if err1 != nil || err2 != nil || err3 != nil {
		return defaultStr
	}
	return strconv.FormatInt(r, 10) + ", " + strconv.FormatInt(g, 10) + ", " + strconv.FormatInt(b, 10)
}

func (u *User) EffectiveAvatarColor() string {
	if u != nil && u.HasPulseProOrHigher() && isValidHexColor(u.AvatarColor) {
		return u.AvatarColor
	}
	palette := []string{"#2F7BFF", "#07baae", "#e7064f", "#8b5cf6", "#f59e0b", "#10b981"}
	if u == nil || u.ID <= 0 {
		return palette[0]
	}
	return palette[int(u.ID)%len(palette)]
}

func (u *User) EffectiveBannerColor() string {
	if u != nil && u.HasPulseProOrHigher() {
		if isValidHexColor(u.BannerColor) {
			return u.BannerColor
		}
		if isValidHexColor(u.ThemePrimary) {
			return u.ThemePrimary
		}
	}
	return "#1e293b"
}

func (u *User) EffectiveThemePrimary() string {
	if u != nil && u.HasPulseProOrHigher() {
		if isValidHexColor(u.ThemePrimary) {
			return u.ThemePrimary
		}
		if isValidHexColor(u.BannerColor) {
			return u.BannerColor
		}
	}
	return "#1d3557"
}

func (u *User) EffectiveThemeAccent() string {
	if u != nil && u.HasPulseProOrHigher() {
		if isValidHexColor(u.ThemeAccent) {
			return u.ThemeAccent
		}
		if isValidHexColor(u.AvatarColor) {
			return u.AvatarColor
		}
	}
	return "#2F7BFF"
}

func (u *User) ThemePrimaryRGB() string {
	return hexToRGBString(u.EffectiveThemePrimary(), 29, 53, 87)
}

func (u *User) ThemeAccentRGB() string {
	return hexToRGBString(u.EffectiveThemeAccent(), 47, 123, 255)
}

func (u *User) BannerColorRGB() string {
	return hexToRGBString(u.EffectiveBannerColor(), 30, 41, 59)
}

func (u *User) AvatarColorRGB() string {
	return hexToRGBString(u.EffectiveAvatarColor(), 47, 123, 255)
}

func (u *User) ThemeStyleAttr() template.CSS {
	return template.CSS(
		"--prof-primary: " + u.EffectiveThemePrimary() +
			"; --prof-accent: " + u.EffectiveThemeAccent() +
			"; --prof-primary-rgb: " + u.ThemePrimaryRGB() +
			"; --prof-accent-rgb: " + u.ThemeAccentRGB() +
			"; --prof-banner: " + u.EffectiveBannerColor() +
			"; --prof-avatar: " + u.EffectiveAvatarColor() + ";",
	)
}

func (u *User) BannerPath() string {
	if u == nil {
		return ""
	}
	if strings.HasPrefix(u.BannerData, "data:image/") {
		v := u.UpdatedAt
		if v == 0 {
			v = u.CreatedAt
		}
		return "/banners/u/" + url.PathEscape(u.Username) + "?v=" + strconv.FormatInt(v, 10)
	}
	if u.BannerURL != "" && !strings.ContainsAny(u.BannerURL, `"'<>()\`) {
		return u.BannerURL
	}
	return ""
}

func (u *User) BannerStyleAttr() template.CSS {
	base := "background: linear-gradient(135deg, var(--prof-banner) 0%, var(--prof-primary) 52%, var(--prof-accent) 100%), url('/img/nsm-backdrop.png') center/256px repeat;"
	if bp := u.BannerPath(); bp != "" {
		base += "background-image: linear-gradient(180deg, rgba(14,16,22,0.18), rgba(14,16,22,0.82)), url('" + bp + "');background-size:cover;background-position:center;"
	}
	return template.CSS(base)
}

func (u *User) AvatarRingStyleAttr() template.CSS {
	return template.CSS("border-radius:50%;border:2px solid " + u.EffectiveThemePrimary() + ";")
}

func (u *User) AvatarPath() string {
	if u == nil {
		return "/avatars/u/guest/120.png"
	}
	if !strings.HasPrefix(u.AvatarData, "data:image/") && u.AvatarURL != "" && !strings.ContainsAny(u.AvatarURL, `"'<>()\`) {
		return u.AvatarURL
	}
	v := u.UpdatedAt
	if v == 0 {
		v = u.CreatedAt
	}
	return "/avatars/u/" + url.PathEscape(u.Username) + "/120.png?v=" + strconv.FormatInt(v, 10)
}

func (u *User) AvatarTemplate() string {
	if u == nil {
		return "/avatars/u/guest/{size}.png"
	}
	v := u.UpdatedAt
	if v == 0 {
		v = u.CreatedAt
	}
	return "/avatars/u/" + url.PathEscape(u.Username) + "/{size}.png?v=" + strconv.FormatInt(v, 10)
}

type Session struct {
	UserID  int64 `json:"userId"`
	Expires int64 `json:"expires"`
}

type ClientToken struct {
	Token     string `json:"token"`
	UserID    int64  `json:"userId"`
	ClientID  string `json:"clientId"`
	AppName   string `json:"appName,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	LastUsed  int64  `json:"lastUsed,omitempty"`
}

type DirectMessage struct {
	ID         int64  `json:"id"`
	FromUserID int64  `json:"fromUserId"`
	FromUser   string `json:"fromUser"`
	ToUserID   int64  `json:"toUserId"`
	ToUser     string `json:"toUser"`
	Body       string `json:"body"`
	CreatedAt  int64  `json:"createdAt"`
	Read       bool   `json:"read,omitempty"`
}

type UserNotification struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"userId"`
	Username  string `json:"username"`
	Kind      string `json:"kind"` // "message", "replied", "liked", "badge"
	Title     string `json:"title"`
	Excerpt   string `json:"excerpt"`
	URL       string `json:"url"`
	CreatedAt int64  `json:"createdAt"`
	Unread    bool   `json:"unread"`
}

type SupportReply struct {
	ID             int64  `json:"id"`
	AuthorUsername string `json:"authorUsername"`
	AuthorDisplay  string `json:"authorDisplay"`
	AuthorRole     string `json:"authorRole"`
	AuthorAvatar   string `json:"authorAvatar"`
	IsStaff        bool   `json:"isStaff"`
	Body           string `json:"body"`
	CreatedAt      int64  `json:"createdAt"`
}

type SupportTicket struct {
	ID                int64          `json:"id"`
	RequesterID       int64          `json:"requesterId"`
	RequesterUsername string         `json:"requesterUsername"`
	RequesterDisplay  string         `json:"requesterDisplay"`
	RequesterEmail    string         `json:"requesterEmail"`
	Subject           string         `json:"subject"`
	Category          string         `json:"category"`
	Priority          string         `json:"priority"`
	ServerEndpoint    string         `json:"serverEndpoint,omitempty"`
	Status            string         `json:"status"` // "Open", "Answered", "Solved", "Closed"
	CreatedAt         int64          `json:"createdAt"`
	UpdatedAt         int64          `json:"updatedAt"`
	ClosedAt          int64          `json:"closedAt,omitempty"`
	ClosedBy          string         `json:"closedBy,omitempty"`
	Messages          []SupportReply `json:"messages"`
}

type Accounts struct {
	mu               sync.Mutex
	Users            []User                 `json:"users"`
	Sessions         map[string]Session     `json:"sessions"`
	ClientTokens     map[string]ClientToken `json:"clientTokens,omitempty"`
	Upvotes          map[string][]int64     `json:"upvotes"` // endpoint -> user ids
	FeaturedServers  []string               `json:"featuredServers,omitempty"`
	ServerKeys       []ServerKey            `json:"serverKeys,omitempty"`
	DirectMessages   []DirectMessage        `json:"directMessages,omitempty"`
	Notifications    []UserNotification     `json:"notifications,omitempty"`
	SupportTickets   []SupportTicket        `json:"supportTickets,omitempty"`
	OwnerPasswordSet bool                   `json:"ownerPasswordSet,omitempty"`
	NextID           int64                  `json:"nextId"`
	NextMsgID        int64                  `json:"nextMsgId,omitempty"`
	NextNotifID      int64                  `json:"nextNotifId,omitempty"`
	NextTicketID     int64                  `json:"nextTicketId,omitempty"`
}

var (
	accMu   sync.Mutex
	acc     *Accounts
	accPath = "accounts.json"
)

func loadAccounts() {
	accMu.Lock()
	defer accMu.Unlock()
	acc = &Accounts{
		Sessions:     map[string]Session{},
		ClientTokens: map[string]ClientToken{},
		Upvotes:      map[string][]int64{},
		NextID:       1,
		NextTicketID: 1001,
	}
	if b, err := os.ReadFile(accPath); err == nil {
		json.Unmarshal(b, acc)
	}
	if acc.Sessions == nil {
		acc.Sessions = map[string]Session{}
	}
	if acc.ClientTokens == nil {
		acc.ClientTokens = map[string]ClientToken{}
	}
	if acc.Upvotes == nil {
		acc.Upvotes = map[string][]int64{}
	}
	if acc.NextTicketID < 1001 {
		acc.NextTicketID = int64(1001 + len(acc.SupportTickets))
	}
	if len(acc.Users) == 0 {
		acc.Users = append(acc.Users, User{
			ID:               1,
			Username:         "nta~",
			DisplayName:      "nta~",
			Title:            "NSM Founder",
			Bio:              "Founder & Lead Developer of the NSM Multiplayer Platform.",
			Signature:        "NSM - Powered by AFX | Official Platform Founder",
			AvatarColor:      "#2F7BFF",
			BannerColor:      "#1d3557",
			ThemePrimary:     "#1d3557",
			ThemeAccent:      "#2F7BFF",
			AvatarDecoration: "cat_ears",
			ProfileEffect:    "aurora_waves",
			PulseTier:        "eternal",
			PulseSince:       time.Now().UnixMilli(),
			IsPartner:        true,
			SquadHouse:       "all",
			Location:         "United States",
			DiscordTag:       "nta~",
			GitHub:           "platinum-cfx",
			Website:          "https://github.com/platinum-cfx/NSM-FIVEM",
			FavoriteServer:   "NSM Official Server",
			Email:            "nta@owner.local",
			EmailVerified:    true,
			Role:             "owner",
			CreatedAt:        time.Now().UnixMilli(),
		})
		acc.NextID = 2
	}
	for i := range acc.Users {
		if acc.Users[i].ID == 1 || strings.EqualFold(acc.Users[i].Username, ownerName) || strings.EqualFold(acc.Users[i].Username, "nta~") || strings.EqualFold(acc.Users[i].Username, "Cravenz") {
			if strings.EqualFold(acc.Users[i].Username, "Cravenz") || acc.Users[i].Username == "" {
				acc.Users[i].Username = "nta~"
			}
			if strings.EqualFold(acc.Users[i].DisplayName, "Cravenz") || acc.Users[i].DisplayName == "" {
				acc.Users[i].DisplayName = acc.Users[i].Username
			}
			if acc.Users[i].Role == "" {
				acc.Users[i].Role = "owner"
			}
			acc.Users[i].PulseTier = "eternal"
			acc.Users[i].IsPartner = true
			acc.Users[i].EmailVerified = true
			acc.Users[i].Banned = false
			if acc.Users[i].Pass != "" {
				acc.OwnerPasswordSet = true
			}
			if acc.Users[i].AvatarDecoration == "" {
				acc.Users[i].AvatarDecoration = "cat_ears"
			}
			if acc.Users[i].ProfileEffect == "" {
				acc.Users[i].ProfileEffect = "aurora_waves"
			}
			if acc.Users[i].SquadHouse == "" {
				acc.Users[i].SquadHouse = "all"
			}
			if acc.Users[i].DiscordTag == "" || strings.EqualFold(acc.Users[i].DiscordTag, "cravenz") {
				acc.Users[i].DiscordTag = "nta~"
			}
			if acc.Users[i].GitHub == "" {
				acc.Users[i].GitHub = "platinum-cfx"
			}
		}
		if acc.Users[i].ID >= acc.NextID {
			acc.NextID = acc.Users[i].ID + 1
		}
	}
	for ki := range acc.ServerKeys {
		if strings.EqualFold(acc.ServerKeys[ki].Username, "Cravenz") {
			acc.ServerKeys[ki].Username = "nta~"
		}
	}
	for mi := range acc.DirectMessages {
		if strings.EqualFold(acc.DirectMessages[mi].FromUser, "Cravenz") {
			acc.DirectMessages[mi].FromUser = "nta~"
		}
		if strings.EqualFold(acc.DirectMessages[mi].ToUser, "Cravenz") {
			acc.DirectMessages[mi].ToUser = "nta~"
		}
	}
	for ni := range acc.Notifications {
		if strings.EqualFold(acc.Notifications[ni].Username, "Cravenz") {
			acc.Notifications[ni].Username = "nta~"
		}
	}
	saveAccountsLocked()
}

func ownerNeedsPasswordSetupLocked() bool {
	if acc == nil {
		return false
	}
	if acc.OwnerPasswordSet {
		return false
	}
	for i := range acc.Users {
		if acc.Users[i].IsFounderAccount() {
			return acc.Users[i].Pass == ""
		}
	}
	return true
}

func ownerNeedsPasswordSetup() bool {
	accMu.Lock()
	defer accMu.Unlock()
	return ownerNeedsPasswordSetupLocked()
}

func saveAccountsLocked() {
	b, _ := json.MarshalIndent(acc, "", "  ")
	tmp := accPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		log.Printf("accounts save error: %v", err)
		_ = os.WriteFile(accPath, b, 0600)
		return
	}
	if err := os.Rename(tmp, accPath); err != nil {
		_ = os.WriteFile(accPath, b, 0600)
		_ = os.Remove(tmp)
	}
}

func findUser(name string) *User {
	name = strings.TrimPrefix(strings.TrimSpace(name), "@")
	for i := range acc.Users {
		if strings.EqualFold(acc.Users[i].Username, name) {
			return &acc.Users[i]
		}
	}
	if strings.EqualFold(name, "Cravenz") || strings.EqualFold(name, "nta~") || strings.EqualFold(name, ownerName) {
		for i := range acc.Users {
			if acc.Users[i].ID == 1 || acc.Users[i].Role == "owner" {
				return &acc.Users[i]
			}
		}
	}
	return nil
}

func findUserByLoginOrEmail(ident string) *User {
	ident = strings.TrimSpace(ident)
	if ident == "" {
		return nil
	}
	if u := findUser(ident); u != nil {
		return u
	}
	for i := range acc.Users {
		if acc.Users[i].Email != "" && strings.EqualFold(acc.Users[i].Email, ident) {
			return &acc.Users[i]
		}
	}
	return nil
}

func userByID(id int64) *User {
	for i := range acc.Users {
		if acc.Users[i].ID == id {
			return &acc.Users[i]
		}
	}
	return nil
}

func newSession(userID int64) string {
	b := make([]byte, 32)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	acc.Sessions[tok] = Session{UserID: userID, Expires: time.Now().Add(30 * 24 * time.Hour).Unix()}
	return tok
}

func currentUser(r *http.Request) *User {
	tok := ""
	if c, err := r.Cookie("nsm_session"); err == nil && c.Value != "" {
		tok = c.Value
	} else if hdr := strings.TrimSpace(r.Header.Get("X-NSM-Session")); hdr != "" {
		tok = hdr
	}
	if tok == "" {
		return nil
	}
	accMu.Lock()
	defer accMu.Unlock()
	if acc == nil {
		return nil
	}
	s, ok := acc.Sessions[tok]
	if !ok || s.Expires < time.Now().Unix() {
		return nil
	}
	u := userByID(s.UserID)
	if u != nil && u.Banned && !u.IsFounderAccount() {
		delete(acc.Sessions, tok)
		saveAccountsLocked()
		return nil
	}
	return u
}

func roleAtLeast(role, min string) bool {
	rank := map[string]int{"user": 0, "staff": 1, "admin": 2, "owner": 3}
	return rank[role] >= rank[min]
}

func wantsJSONResponse(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/json") {
		return true
	}
	accept := strings.ToLower(r.Header.Get("Accept"))
	if strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html") {
		return true
	}
	return false
}

func safeRedirectTarget(next string) string {
	next = strings.TrimSpace(next)
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/account"
	}
	return next
}

// ---- JSON API & HTML Form Handlers for Login / Register ----

func userPublicMap(u *User) map[string]interface{} {
	if u == nil {
		return nil
	}
	return map[string]interface{}{
		"id":             u.ID,
		"username":       u.Username,
		"displayName":    u.EffectiveDisplayName(),
		"title":          u.EffectiveTitle(),
		"bio":            u.Bio,
		"avatarUrl":      u.AvatarPath(),
		"avatarTemplate": u.AvatarTemplate(),
		"avatarColor":    u.EffectiveAvatarColor(),
		"bannerColor":    u.EffectiveBannerColor(),
		"bannerUrl":      u.BannerURL,
		"location":       u.Location,
		"website":        u.Website,
		"discordTag":     u.DiscordTag,
		"favoriteServer": u.FavoriteServer,
		"email":          u.Email,
		"role":           u.Role,
		"createdAt":      u.CreatedAt,
	}
}

func sendAccountJSON(w http.ResponseWriter, ok bool, msg string, u *User) {
	w.Header().Set("Content-Type", "application/json")
	out := map[string]interface{}{"ok": ok}
	if msg != "" {
		out["error"] = msg
	}
	if u != nil {
		out["user"] = userPublicMap(u)
	}
	json.NewEncoder(w).Encode(out)
}

func generateSixDigitCode() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	val := (int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])) & 0x7fffffff
	return fmt.Sprintf("%06d", 100000+(val%900000))
}

func apiRegister(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password, Email, Next string }
	isJSON := wantsJSONResponse(r)
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "json") {
		json.NewDecoder(r.Body).Decode(&req)
	} else {
		req.Username = r.FormValue("username")
		req.Password = r.FormValue("password")
		req.Email = r.FormValue("email")
		req.Next = r.FormValue("next")
	}
	un := strings.TrimSpace(req.Username)
	emailAddr := strings.TrimSpace(req.Email)
	if len(un) < 3 || len(req.Password) < 6 {
		msg := "Username needs 3+ characters and password needs 6+ characters."
		if isJSON {
			sendAccountJSON(w, false, "username needs 3+ chars, password 6+ chars", nil)
			return
		}
		renderPage(w, r, "register.tmpl", map[string]interface{}{"Title": "Create account", "Error": msg, "Next": req.Next, "PrefillUser": un, "PrefillEmail": emailAddr})
		return
	}
	if !validEmailAddress(emailAddr) {
		msg := "A valid email address is required so we can send your 6-digit verification code."
		if isJSON {
			sendAccountJSON(w, false, "a valid email address is required", nil)
			return
		}
		renderPage(w, r, "register.tmpl", map[string]interface{}{"Title": "Create account", "Error": msg, "Next": req.Next, "PrefillUser": un, "PrefillEmail": emailAddr})
		return
	}
	accMu.Lock()
	if existing := findUser(un); existing != nil {
		// If the seeded owner account (nta~) has not had a password set yet, allow claiming it once.
		if existing.Pass == "" && !acc.OwnerPasswordSet && len(req.Password) >= 6 {
			h, _ := bcrypt.GenerateFromPassword([]byte(req.Password), 10)
			existing.Pass = string(h)
			acc.OwnerPasswordSet = true
			existing.EmailVerified = true
			if strings.TrimSpace(req.Email) != "" {
				existing.Email = strings.TrimSpace(req.Email)
			}
			now := time.Now().UnixMilli()
			existing.UpdatedAt = now
			existing.LastLogin = now
			tok := newSession(existing.ID)
			userCopy := *existing
			saveAccountsLocked()
			accMu.Unlock()
			log.Printf("[accounts] claimed seeded account %s (role %s)", userCopy.Username, userCopy.Role)
			http.SetCookie(w, &http.Cookie{Name: "nsm_session", Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600})
			recordWebsiteLoginForClientAuth(userCopy.ID)
			if isJSON {
				sendAccountJSON(w, true, "", &userCopy)
				return
			}
			http.Redirect(w, r, safeRedirectTarget(req.Next), http.StatusSeeOther)
			return
		}
		accMu.Unlock()
		if isJSON {
			sendAccountJSON(w, false, "that username is taken", nil)
			return
		}
		renderPage(w, r, "register.tmpl", map[string]interface{}{"Title": "Create account", "Error": "That username is already taken.", "Next": req.Next, "PrefillEmail": req.Email})
		return
	}
	role := "user"
	if strings.EqualFold(un, ownerName) || strings.EqualFold(un, "nta~") {
		role = "owner"
	}
	h, _ := bcrypt.GenerateFromPassword([]byte(req.Password), 10)
	now := time.Now().UnixMilli()
	verifyCode := generateSixDigitCode()
	u := User{
		ID:              acc.NextID,
		Username:        un,
		DisplayName:     un,
		Email:           emailAddr,
		EmailVerified:   role == "owner",
		EmailVerifyCode: verifyCode,
		Pass:            string(h),
		Role:            role,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastLogin:       now,
	}
	acc.NextID++
	acc.Users = append(acc.Users, u)
	addUserNotificationLocked(&acc.Users[len(acc.Users)-1], "badge",
		"Verify Your Email Address ("+emailAddr+")",
		"Your 6-digit AFX Email Verification Code is: "+verifyCode,
		"/verify-email")
	tok := newSession(u.ID)
	saveAccountsLocked()
	accMu.Unlock()

	mailSent := dispatchVerificationEmail(emailAddr, un, verifyCode)
	verifyMsg := "Your 6-digit verification code was emailed to " + emailAddr + "."
	if !mailSent {
		verifyMsg = "Your 6-digit verification code could not be emailed (mail delivery is not configured on this server) — it is also in your in-app notifications."
	}
	log.Printf("[accounts] registered %s (id %d, mail sent: %v)", un, u.ID, mailSent)
	http.SetCookie(w, &http.Cookie{Name: "nsm_session", Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600})
	recordWebsiteLoginForClientAuth(u.ID)

	if isJSON {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":                 true,
			"verificationRequired": !u.EmailVerified,
			"message":            verifyMsg,
			"user":               userPublicMap(&u),
		})
		return
	}
	if !u.EmailVerified {
		next := strings.TrimSpace(req.Next)
		if next == "" {
			next = "/u/" + url.PathEscape(u.Username)
		}
		http.Redirect(w, r, "/verify-email?next="+url.QueryEscape(next)+"&msg="+url.QueryEscape(verifyMsg), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, safeRedirectTarget(req.Next), http.StatusSeeOther)
}

func apiLogin(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password, TotpCode, Next string }
	isJSON := wantsJSONResponse(r)
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "json") {
		json.NewDecoder(r.Body).Decode(&req)
	} else {
		req.Username = r.FormValue("username")
		if req.Username == "" {
			req.Username = r.FormValue("login")
		}
		req.Password = r.FormValue("password")
		req.TotpCode = strings.TrimSpace(r.FormValue("totp_code"))
		req.Next = r.FormValue("next")
	}
	accMu.Lock()
	u := findUserByLoginOrEmail(strings.TrimSpace(req.Username))
	if u != nil && u.Banned && !u.IsFounderAccount() {
		reason := u.BanReason
		if reason == "" {
			reason = "Violation of AFX Community Guidelines."
		}
		accMu.Unlock()
		msg := "This account has been suspended by AFX Staff. Reason: " + reason
		if isJSON {
			sendAccountJSON(w, false, msg, nil)
			return
		}
		renderPage(w, r, "login.tmpl", map[string]interface{}{"Title": "Log in", "Error": msg, "Next": req.Next, "PrefillUser": req.Username, "ShowOwnerSetup": ownerNeedsPasswordSetup()})
		return
	}
	if u != nil && u.Pass == "" && !acc.OwnerPasswordSet && len(req.Password) >= 1 {
		// Seeded owner account (nta~) had no password set yet: initialize it once on first login.
		h, _ := bcrypt.GenerateFromPassword([]byte(req.Password), 10)
		u.Pass = string(h)
		acc.OwnerPasswordSet = true
		u.UpdatedAt = time.Now().UnixMilli()
		log.Printf("[accounts] initialized one-time password on first login for %s (role %s)", u.Username, u.Role)
	}
	if u == nil || bcrypt.CompareHashAndPassword([]byte(u.Pass), []byte(req.Password)) != nil {
		showSetup := ownerNeedsPasswordSetupLocked()
		accMu.Unlock()
		if isJSON {
			sendAccountJSON(w, false, "wrong username or password", nil)
			return
		}
		renderPage(w, r, "login.tmpl", map[string]interface{}{"Title": "Log in", "Error": "Wrong username or password.", "Next": req.Next, "PrefillUser": req.Username, "ShowOwnerSetup": showSetup})
		return
	}
	if u.TwoFactorEnabled && u.TwoFactorCode != "" {
		if req.TotpCode == "" {
			showSetup := ownerNeedsPasswordSetupLocked()
			accMu.Unlock()
			if isJSON {
				sendAccountJSON(w, false, "2fa_required", nil)
				return
			}
			renderPage(w, r, "login.tmpl", map[string]interface{}{
				"Title":           "Two-Step Verification",
				"Require2FA":      true,
				"PrefillUser":     u.Username,
				"PendingPassword": req.Password,
				"Next":            req.Next,
				"ShowOwnerSetup":  showSetup,
			})
			return
		}
		if req.TotpCode != u.TwoFactorCode {
			showSetup := ownerNeedsPasswordSetupLocked()
			accMu.Unlock()
			if isJSON {
				sendAccountJSON(w, false, "invalid 2fa code", nil)
				return
			}
			renderPage(w, r, "login.tmpl", map[string]interface{}{
				"Title":           "Two-Step Verification",
				"Require2FA":      true,
				"Error":           "Invalid 6-digit Two-Step Verification code. Please try again.",
				"PrefillUser":     u.Username,
				"PendingPassword": req.Password,
				"Next":            req.Next,
				"ShowOwnerSetup":  showSetup,
			})
			return
		}
	}
	u.LastLogin = time.Now().UnixMilli()
	tok := newSession(u.ID)
	userCopy := *u
	saveAccountsLocked()
	accMu.Unlock()

	log.Printf("[accounts] login %s (role %s)", userCopy.Username, userCopy.Role)
	http.SetCookie(w, &http.Cookie{Name: "nsm_session", Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600})
	recordWebsiteLoginForClientAuth(userCopy.ID)

	if isJSON {
		sendAccountJSON(w, true, "", &userCopy)
		return
	}
	http.Redirect(w, r, safeRedirectTarget(req.Next), http.StatusSeeOther)
}

func isLoopbackClient(r *http.Request) bool {
	if r.Header.Get("X-NSM-Forwarded") == "1" {
		return false
	}
	ip := clientIP(r)
	return ip == "127.0.0.1" || ip == "::1" || ip == "localhost" || strings.HasPrefix(ip, "127.")
}

// accountClaimOwner is a one-time setup popup on 127.0.0.1 to set the initial password for the founder account (nta~).
// Once the password has been set once, this popup is permanently hidden.
func accountClaimOwner(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackClient(r) {
		http.Error(w, "loopback only", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	pw := r.FormValue("password")
	next := r.FormValue("next")

	accMu.Lock()
	if !ownerNeedsPasswordSetupLocked() {
		accMu.Unlock()
		if wantsJSONResponse(r) {
			w.WriteHeader(http.StatusForbidden)
			sendAccountJSON(w, false, "Owner password for nta~ has already been set.", nil)
			return
		}
		renderPage(w, r, "login.tmpl", map[string]interface{}{
			"Title":          "Log in",
			"Error":          "Owner password for nta~ has already been set. Please sign in with your password below.",
			"PrefillUser":    ownerName,
			"Next":           next,
			"ShowOwnerSetup": false,
		})
		return
	}
	if len(pw) < 6 {
		accMu.Unlock()
		renderPage(w, r, "login.tmpl", map[string]interface{}{
			"Title":          "Log in",
			"Error":          "Please enter a password of at least 6 characters for nta~ (one-time setup).",
			"Next":           next,
			"ShowOwnerSetup": true,
		})
		return
	}

	u := findUser(ownerName)
	now := time.Now().UnixMilli()
	if u == nil {
		acc.Users = append(acc.Users, User{
			ID:            acc.NextID,
			Username:      ownerName,
			DisplayName:   ownerName,
			Title:         "NSM Founder & Owner",
			Bio:           "Founder & Lead Developer of the NSM Multiplayer Platform.",
			AvatarColor:   "#2F7BFF",
			BannerColor:   "#1d3557",
			Email:         "nta@owner.local",
			EmailVerified: true,
			Role:          "owner",
			CreatedAt:     now,
			UpdatedAt:     now,
			LastLogin:     now,
		})
		acc.NextID++
		u = findUser(ownerName)
	}
	u.Role = "owner"
	u.EmailVerified = true
	h, _ := bcrypt.GenerateFromPassword([]byte(pw), 10)
	u.Pass = string(h)
	acc.OwnerPasswordSet = true
	u.UpdatedAt = now
	u.LastLogin = now
	tok := newSession(u.ID)
	userCopy := *u
	saveAccountsLocked()
	accMu.Unlock()

	log.Printf("[accounts] completed one-time owner password setup for %s (id %d)", userCopy.Username, userCopy.ID)
	http.SetCookie(w, &http.Cookie{Name: "nsm_session", Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600})
	recordWebsiteLoginForClientAuth(userCopy.ID)

	if wantsJSONResponse(r) {
		sendAccountJSON(w, true, "", &userCopy)
		return
	}
	http.Redirect(w, r, safeRedirectTarget(next), http.StatusSeeOther)
}

func verifyEmailPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login?next=/verify-email", http.StatusSeeOther)
		return
	}
	next := r.URL.Query().Get("next")
	msg := r.URL.Query().Get("msg")
	errMsg := r.URL.Query().Get("error")
	accMu.Lock()
	target := userByID(u.ID)
	var userCopy User
	if target != nil {
		if !target.EmailVerified && target.EmailVerifyCode == "" {
			target.EmailVerifyCode = generateSixDigitCode()
			saveAccountsLocked()
		}
		userCopy = *target
	}
	accMu.Unlock()
	renderPage(w, r, "verify_email.tmpl", map[string]interface{}{
		"Title":      "Verify Your Email - AFX Account Security",
		"User":       &userCopy,
		"Next":       next,
		"Msg":        msg,
		"Error":      errMsg,
		"IsLoopback": isLoopbackClient(r),
	})
}

func accountVerifyEmail(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	code := strings.TrimSpace(r.FormValue("code"))
	next := strings.TrimSpace(r.FormValue("next"))
	if next == "" {
		next = strings.TrimSpace(r.FormValue("return_to"))
	}

	accMu.Lock()
	target := userByID(u.ID)
	if target == nil {
		accMu.Unlock()
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if target.EmailVerified {
		userCopy := *target
		accMu.Unlock()
		if next != "" {
			http.Redirect(w, r, safeRedirectTarget(next), http.StatusSeeOther)
			return
		}
		data := buildDiscourseProfileData(&userCopy, &userCopy, "preferences", "Your email address is verified.", "")
		renderPage(w, r, "profile.tmpl", data)
		return
	}
	if code == "" || code != target.EmailVerifyCode {
		userCopy := *target
		accMu.Unlock()
		renderPage(w, r, "verify_email.tmpl", map[string]interface{}{
			"Title":      "Verify Your Email - AFX Account Security",
			"User":       &userCopy,
			"Next":       next,
			"Error":      "Invalid 6-digit verification code. Please check the code sent to " + userCopy.Email + " and try again.",
			"IsLoopback": isLoopbackClient(r),
		})
		return
	}
	target.EmailVerified = true
	target.EmailVerifyCode = ""
	target.UpdatedAt = time.Now().UnixMilli()
	userCopy := *target
	saveAccountsLocked()
	accMu.Unlock()

	if next != "" {
		http.Redirect(w, r, safeRedirectTarget(next), http.StatusSeeOther)
		return
	}
	data := buildDiscourseProfileData(&userCopy, &userCopy, "preferences", "Email address "+userCopy.Email+" verified!", "")
	renderPage(w, r, "profile.tmpl", data)
}

func accountResendVerifyEmail(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	next := strings.TrimSpace(r.FormValue("next"))
	newEmail := strings.TrimSpace(r.FormValue("email"))

	accMu.Lock()
	target := userByID(u.ID)
	if target == nil {
		accMu.Unlock()
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if newEmail != "" && !strings.EqualFold(newEmail, target.Email) {
		if !validEmailAddress(newEmail) {
			accMu.Unlock()
			http.Redirect(w, r, "/verify-email?error="+url.QueryEscape("Please provide a valid email address.")+"&next="+url.QueryEscape(next), http.StatusSeeOther)
			return
		}
		target.Email = newEmail
		target.EmailVerified = false
	}
	code := generateSixDigitCode()
	target.EmailVerifyCode = code
	addUserNotificationLocked(target, "badge",
		"New Email Verification Code ("+target.Email+")",
		"Your 6-digit AFX Email Verification Code is: "+code,
		"/verify-email")
	emailAddr := target.Email
	un := target.Username
	saveAccountsLocked()
	accMu.Unlock()

	mailSent := dispatchVerificationEmail(emailAddr, un, code)
	resendMsg := "A fresh 6-digit verification code has been emailed to " + emailAddr + "."
	if !mailSent {
		resendMsg = "Your new 6-digit verification code could not be emailed (mail delivery is not configured on this server) — it is also in your in-app notifications."
	}
	http.Redirect(w, r, "/verify-email?msg="+url.QueryEscape(resendMsg)+"&next="+url.QueryEscape(next), http.StatusSeeOther)
}

func accountConfigure2FA(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	action := strings.ToLower(strings.TrimSpace(r.FormValue("action")))
	code := strings.TrimSpace(r.FormValue("code"))

	accMu.Lock()
	target := userByID(u.ID)
	if target == nil {
		accMu.Unlock()
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	var msg, errMsg string
	if action == "disable" {
		target.TwoFactorEnabled = false
		target.TwoFactorCode = ""
		target.UpdatedAt = time.Now().UnixMilli()
		saveAccountsLocked()
		msg = "Two-Step Verification (2FA) has been disabled on your account."
	} else {
		validDigits := len(code) == 6
		for i := 0; i < len(code); i++ {
			if code[i] < '0' || code[i] > '9' {
				validDigits = false
			}
		}
		if !validDigits {
			errMsg = "Please enter a 6-digit numeric security code (e.g. 482910) to enable Two-Step Verification."
		} else {
			target.TwoFactorEnabled = true
			target.TwoFactorCode = code
			target.UpdatedAt = time.Now().UnixMilli()
			saveAccountsLocked()
			msg = "Two-Step Verification (2FA) is now ENABLED! You will be asked for your 6-digit code whenever you sign in."
		}
	}
	userCopy := *target
	accMu.Unlock()

	data := buildDiscourseProfileData(&userCopy, &userCopy, "preferences", msg, errMsg)
	renderPage(w, r, "profile.tmpl", data)
}

func apiLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("nsm_session"); err == nil {
		accMu.Lock()
		delete(acc.Sessions, c.Value)
		saveAccountsLocked()
		accMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "nsm_session", Value: "", Path: "/", MaxAge: -1})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func apiMe(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	sendAccountJSON(w, u != nil, "", u)
}

func accountChangePassword(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	old, nw := r.FormValue("old"), r.FormValue("new")
	accMu.Lock()
	target := userByID(u.ID)
	if target == nil || bcrypt.CompareHashAndPassword([]byte(target.Pass), []byte(old)) != nil || len(nw) < 6 {
		accMu.Unlock()
		renderAccountPageWithMsg(w, r, u, "", "Current password is wrong, or new password is shorter than 6 characters.")
		return
	}
	h, _ := bcrypt.GenerateFromPassword([]byte(nw), 10)
	target.Pass = string(h)
	target.UpdatedAt = time.Now().UnixMilli()
	userCopy := *target
	saveAccountsLocked()
	accMu.Unlock()
	renderAccountPageWithMsg(w, r, &userCopy, "Password updated successfully.", "")
}

func accountUpdateProfile(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		if wantsJSONResponse(r) {
			sendAccountJSON(w, false, "login required", nil)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		_ = r.ParseMultipartForm(16 << 20)
	} else {
		_ = r.ParseForm()
	}

	firstNonEmpty := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(r.FormValue(k)); v != "" {
				return v
			}
		}
		return ""
	}
	newUsername := firstNonEmpty("new_username", "username")
	displayName := firstNonEmpty("display_name", "displayName")
	title := firstNonEmpty("title")
	bio := firstNonEmpty("bio")
	signature := firstNonEmpty("signature")
	avatarURL := firstNonEmpty("avatar_url", "avatarUrl")
	avatarColor := firstNonEmpty("avatar_color", "avatarColor")
	bannerColor := firstNonEmpty("banner_color", "bannerColor")
	bannerURL := firstNonEmpty("banner_url", "bannerUrl")
	themePrimary := firstNonEmpty("theme_primary", "themePrimary")
	themeAccent := firstNonEmpty("theme_accent", "themeAccent")
	avatarDecoration := firstNonEmpty("avatar_decoration", "avatarDecoration")
	profileEffect := firstNonEmpty("profile_effect", "profileEffect")
	squadHouse := firstNonEmpty("squad_house", "squadHouse")
	location := firstNonEmpty("location")
	website := firstNonEmpty("website")
	discordTag := firstNonEmpty("discord_tag", "discordTag", "discord")
	github := firstNonEmpty("github", "github_url", "githubUrl")
	steam := firstNonEmpty("steam", "steam_id", "steamId")
	youtube := firstNonEmpty("youtube")
	twitch := firstNonEmpty("twitch")
	twitter := firstNonEmpty("twitter", "x_handle")
	favServer := firstNonEmpty("favorite_server", "favoriteServer")
	email := firstNonEmpty("email")
	clearAvatar := r.FormValue("clear_avatar") == "1" || r.FormValue("clearAvatar") == "1"
	clearBanner := r.FormValue("clear_banner") == "1" || r.FormValue("clearBanner") == "1"

	if len(displayName) > 48 {
		displayName = displayName[:48]
	}
	if len(title) > 64 {
		title = title[:64]
	}
	if len(bio) > 600 {
		bio = bio[:600]
	}
	if len(signature) > 200 {
		signature = signature[:200]
	}

	var uploadedAvatarData, uploadedBannerData string
	if ad := firstNonEmpty("avatar_data", "avatarData"); strings.HasPrefix(ad, "data:image/") {
		uploadedAvatarData = ad
	}
	if bd := firstNonEmpty("banner_data", "bannerData"); strings.HasPrefix(bd, "data:image/") {
		uploadedBannerData = bd
	}
	if r.MultipartForm != nil {
		files := r.MultipartForm.File["avatar_file"]
		if len(files) == 0 {
			files = r.MultipartForm.File["avatarFile"]
		}
		if len(files) > 0 {
			f, err := files[0].Open()
			if err == nil {
				defer f.Close()
				raw, err := io.ReadAll(io.LimitReader(f, 8<<20))
				if err == nil && len(raw) > 0 {
					ct := http.DetectContentType(raw)
					if ct == "image/png" || ct == "image/jpeg" || ct == "image/gif" || ct == "image/webp" {
						uploadedAvatarData = "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(raw)
					}
				}
			}
		}
		bfiles := r.MultipartForm.File["banner_file"]
		if len(bfiles) == 0 {
			bfiles = r.MultipartForm.File["bannerFile"]
		}
		if len(bfiles) > 0 {
			bf, err := bfiles[0].Open()
			if err == nil {
				defer bf.Close()
				raw, err := io.ReadAll(io.LimitReader(bf, 10<<20))
				if err == nil && len(raw) > 0 {
					ct := http.DetectContentType(raw)
					if ct == "image/png" || ct == "image/jpeg" || ct == "image/gif" || ct == "image/webp" {
						uploadedBannerData = "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(raw)
					}
				}
			}
		}
	}

	accMu.Lock()
	target := userByID(u.ID)
	if target == nil {
		accMu.Unlock()
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	var oldUsername, updatedUsername, warnMsg string
	cleanNewUsername := strings.TrimPrefix(strings.TrimSpace(newUsername), "@")
	if cleanNewUsername != "" && cleanNewUsername != target.Username {
		if !isValidUsername(cleanNewUsername) {
			warnMsg = "Username must be 3–32 characters (letters, numbers, _, -, or .)."
		} else {
			existing := findUser(cleanNewUsername)
			if existing != nil && existing.ID != target.ID {
				warnMsg = "Username @" + cleanNewUsername + " is already taken."
			} else {
				oldUsername = target.Username
				updatedUsername = cleanNewUsername
				if strings.EqualFold(target.Username, ownerName) {
					ownerName = cleanNewUsername
				}
				// If DisplayName was still set to the previous username, update DisplayName to match the new username
				if displayName == "" || strings.EqualFold(displayName, oldUsername) {
					displayName = cleanNewUsername
				}
				target.Username = cleanNewUsername
				for ki := range acc.ServerKeys {
					if acc.ServerKeys[ki].UserID == target.ID {
						acc.ServerKeys[ki].Username = cleanNewUsername
					}
				}
				for mi := range acc.DirectMessages {
					if acc.DirectMessages[mi].FromUserID == target.ID || strings.EqualFold(acc.DirectMessages[mi].FromUser, oldUsername) {
						acc.DirectMessages[mi].FromUser = cleanNewUsername
					}
					if acc.DirectMessages[mi].ToUserID == target.ID || strings.EqualFold(acc.DirectMessages[mi].ToUser, oldUsername) {
						acc.DirectMessages[mi].ToUser = cleanNewUsername
					}
				}
				for ni := range acc.Notifications {
					if acc.Notifications[ni].UserID == target.ID || strings.EqualFold(acc.Notifications[ni].Username, oldUsername) {
						acc.Notifications[ni].Username = cleanNewUsername
					}
				}
			}
		}
	}
	if displayName != "" || r.Form.Has("display_name") || r.Form.Has("displayName") {
		target.DisplayName = displayName
	}
	target.Title = title
	target.Bio = bio
	if signature != "" || r.Form.Has("signature") {
		target.Signature = signature
	}
	if clearAvatar {
		target.AvatarData = ""
		target.AvatarURL = ""
	} else {
		if uploadedAvatarData != "" {
			target.AvatarData = uploadedAvatarData
		}
		if avatarURL != "" || r.Form.Has("avatarUrl") || r.Form.Has("avatar_url") {
			target.AvatarURL = avatarURL
		}
	}
	if clearBanner {
		target.BannerData = ""
		target.BannerURL = ""
	} else {
		if uploadedBannerData != "" {
			target.BannerData = uploadedBannerData
		}
		if bannerURL != "" || r.Form.Has("bannerUrl") || r.Form.Has("banner_url") {
			target.BannerURL = bannerURL
		}
	}
	// Only AFX Pulse Pro / Sovereign / Founder Eternal can customize Profile Layout Colors & Moving Profile Effects
	if target.HasPulseProOrHigher() {
		if isValidHexColor(avatarColor) {
			target.AvatarColor = avatarColor
			if !isValidHexColor(themeAccent) && target.ThemeAccent == "" {
				target.ThemeAccent = avatarColor
			}
		}
		if isValidHexColor(bannerColor) {
			target.BannerColor = bannerColor
			if !isValidHexColor(themePrimary) && target.ThemePrimary == "" {
				target.ThemePrimary = bannerColor
			}
		}
		if isValidHexColor(themePrimary) {
			target.ThemePrimary = themePrimary
			if !isValidHexColor(bannerColor) {
				target.BannerColor = themePrimary
			}
		}
		if isValidHexColor(themeAccent) {
			target.ThemeAccent = themeAccent
		}
		if profileEffect != "" {
			target.ProfileEffect = strings.ToLower(profileEffect)
		}
	}
	if target.HasPulse() && avatarDecoration != "" {
		target.AvatarDecoration = strings.ToLower(avatarDecoration)
	}
	if squadHouse != "" {
		sh := strings.ToLower(squadHouse)
		if sh == "bravery" || sh == "brilliance" || sh == "balance" || sh == "all" {
			target.SquadHouse = sh
		}
	}
	if target.IsFounderAccount() {
		target.PulseTier = "eternal"
		target.IsPartner = true
	}
	target.Location = location
	target.Website = website
	target.DiscordTag = discordTag
	if github != "" || r.Form.Has("github") {
		target.GitHub = github
	}
	if steam != "" || r.Form.Has("steam") {
		target.Steam = steam
	}
	if youtube != "" || r.Form.Has("youtube") {
		target.YouTube = youtube
	}
	if twitch != "" || r.Form.Has("twitch") {
		target.Twitch = twitch
	}
	if twitter != "" || r.Form.Has("twitter") {
		target.Twitter = twitter
	}
	target.FavoriteServer = favServer
	if email != "" {
		target.Email = email
	}
	target.UpdatedAt = time.Now().UnixMilli()
	userCopy := *target
	saveAccountsLocked()
	accMu.Unlock()

	if oldUsername != "" && updatedUsername != "" {
		forumMu.Lock()
		if forum != nil {
			changedForum := false
			for ti := range forum.Topics {
				if strings.EqualFold(forum.Topics[ti].AuthorUsername, oldUsername) {
					forum.Topics[ti].AuthorUsername = updatedUsername
					changedForum = true
				}
				for pi := range forum.Topics[ti].Posts {
					if strings.EqualFold(forum.Topics[ti].Posts[pi].AuthorUsername, oldUsername) {
						forum.Topics[ti].Posts[pi].AuthorUsername = updatedUsername
						changedForum = true
					}
					for li := range forum.Topics[ti].Posts[pi].Likes {
						if strings.EqualFold(forum.Topics[ti].Posts[pi].Likes[li], oldUsername) {
							forum.Topics[ti].Posts[pi].Likes[li] = updatedUsername
							changedForum = true
						}
					}
				}
			}
			if changedForum {
				saveForumLocked()
			}
		}
		forumMu.Unlock()
	}

	if wantsJSONResponse(r) {
		sendAccountJSON(w, warnMsg == "", warnMsg, &userCopy)
		return
	}
	returnTab := strings.ToLower(strings.TrimSpace(r.FormValue("return_tab")))
	if returnTab == "" {
		returnTab = "preferences"
	}
	okMsg := "Your profile settings have been updated."
	if updatedUsername != "" {
		okMsg = "Username changed to @" + updatedUsername + " and profile updated!"
	}
	data := buildDiscourseProfileData(&userCopy, &userCopy, returnTab, okMsg, warnMsg)
	renderPage(w, r, "profile.tmpl", data)
}

func validatePulsePaymentCard(cardName, cardNumber, cardExp, cardCVC string) error {
	if len(strings.TrimSpace(cardName)) < 2 {
		return fmt.Errorf("please enter the cardholder name")
	}
	var digits []int
	allSame := true
	for i := 0; i < len(cardNumber); i++ {
		c := cardNumber[i]
		if c == ' ' || c == '-' {
			continue
		}
		if c < '0' || c > '9' {
			return fmt.Errorf("card number must contain digits only")
		}
		d := int(c - '0')
		if len(digits) > 0 && d != digits[0] {
			allSame = false
		}
		digits = append(digits, d)
	}
	if len(digits) < 13 || len(digits) > 19 || allSame {
		return fmt.Errorf("please enter a valid 16-digit credit or debit card number")
	}
	sum := 0
	alt := false
	for i := len(digits) - 1; i >= 0; i-- {
		n := digits[i]
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	if sum%10 != 0 {
		return fmt.Errorf("card number failed Luhn verification check")
	}
	expParts := strings.Split(strings.TrimSpace(cardExp), "/")
	if len(expParts) != 2 {
		return fmt.Errorf("expiration date must be in MM/YY format")
	}
	mm, err1 := strconv.Atoi(strings.TrimSpace(expParts[0]))
	yy, err2 := strconv.Atoi(strings.TrimSpace(expParts[1]))
	if err1 != nil || err2 != nil || mm < 1 || mm > 12 {
		return fmt.Errorf("invalid expiration month/year")
	}
	if yy < 100 {
		yy += 2000
	}
	if yy < 2026 || yy > 2045 {
		return fmt.Errorf("card is expired or expiration year is invalid")
	}
	cvc := strings.TrimSpace(cardCVC)
	if len(cvc) < 3 || len(cvc) > 4 {
		return fmt.Errorf("CVC security code must be 3 or 4 digits")
	}
	for i := 0; i < len(cvc); i++ {
		if cvc[i] < '0' || cvc[i] > '9' {
			return fmt.Errorf("CVC security code must contain digits only")
		}
	}
	return nil
}

func accountSubscribePulse(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		if wantsJSONResponse(r) {
			sendAccountJSON(w, false, "login required", nil)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	if !u.IsFounderAccount() {
		closedMsg := "AFX Pulse is currently closed and not open for purchases right now. Stay tuned for the official opening!"
		if wantsJSONResponse(r) {
			w.WriteHeader(http.StatusForbidden)
			sendAccountJSON(w, false, closedMsg, u)
			return
		}
		data := buildDiscourseProfileData(u, u, "pulse", "", closedMsg)
		renderPage(w, r, "profile.tmpl", data)
		return
	}

	accMu.Lock()
	target := userByID(u.ID)
	if target == nil {
		accMu.Unlock()
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	target.PulseTier = "eternal"
	if target.PulseSince == 0 {
		target.PulseSince = time.Now().UnixMilli()
	}
	if target.AvatarDecoration == "" || target.AvatarDecoration == "none" {
		target.AvatarDecoration = "cat_ears"
	}
	if target.ProfileEffect == "" || target.ProfileEffect == "none" {
		target.ProfileEffect = "aurora_waves"
	}
	target.UpdatedAt = time.Now().UnixMilli()
	userCopy := *target
	saveAccountsLocked()
	accMu.Unlock()

	if wantsJSONResponse(r) {
		sendAccountJSON(w, true, "", &userCopy)
		return
	}
	msg := "AFX Pulse Eternal (Founder) is active on @" + userCopy.Username + "."
	data := buildDiscourseProfileData(&userCopy, &userCopy, "pulse", msg, "")
	renderPage(w, r, "profile.tmpl", data)
}

func serveUserBanner(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	accMu.Lock()
	u := findUser(username)
	var bannerData, bannerURL string
	if u != nil {
		bannerData = u.BannerData
		bannerURL = u.BannerURL
	}
	accMu.Unlock()

	if strings.HasPrefix(bannerData, "data:image/") {
		if idx := strings.Index(bannerData, ";base64,"); idx > 0 {
			ct := strings.TrimPrefix(bannerData[:idx], "data:")
			if raw, err := base64.StdEncoding.DecodeString(bannerData[idx+8:]); err == nil && len(raw) > 0 {
				w.Header().Set("Content-Type", ct)
				w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
				w.WriteHeader(http.StatusOK)
				w.Write(raw)
				return
			}
		}
	}
	if strings.HasPrefix(bannerURL, "http://") || strings.HasPrefix(bannerURL, "https://") {
		http.Redirect(w, r, bannerURL, http.StatusFound)
		return
	}
	http.NotFound(w, r)
}

func renderAccountPageWithMsg(w http.ResponseWriter, r *http.Request, u *User, msg, errMsg string) {
	if u == nil {
		u = currentUser(r)
	}
	if u != nil {
		data := buildDiscourseProfileData(u, u, "preferences", msg, errMsg)
		renderPage(w, r, "profile.tmpl", data)
		return
	}
	renderPage(w, r, "account.tmpl", map[string]interface{}{
		"Title":      "My Account & Profile",
		"User":       nil,
		"Msg":        msg,
		"Error":      errMsg,
		"HasPending": hasPendingClientAuth(),
	})
}

// ---- Avatar PNG endpoint (/avatars/u/{username}/{size}) ----

// 5x7 bitmap font for uppercase A-Z and 0-9 so generated avatars look crisp in both browser and NSM client.
var avatarGlyphs = map[byte][7]uint8{
	'A': {0x0E, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11},
	'B': {0x1E, 0x11, 0x11, 0x1E, 0x11, 0x11, 0x1E},
	'C': {0x0E, 0x11, 0x10, 0x10, 0x10, 0x11, 0x0E},
	'D': {0x1C, 0x12, 0x11, 0x11, 0x11, 0x12, 0x1C},
	'E': {0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x1F},
	'F': {0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x10},
	'G': {0x0E, 0x11, 0x10, 0x17, 0x11, 0x11, 0x0F},
	'H': {0x11, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11},
	'I': {0x0E, 0x04, 0x04, 0x04, 0x04, 0x04, 0x0E},
	'J': {0x07, 0x02, 0x02, 0x02, 0x12, 0x12, 0x0C},
	'K': {0x11, 0x12, 0x14, 0x18, 0x14, 0x12, 0x11},
	'L': {0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x1F},
	'M': {0x11, 0x1B, 0x15, 0x15, 0x11, 0x11, 0x11},
	'N': {0x11, 0x19, 0x15, 0x13, 0x11, 0x11, 0x11},
	'O': {0x0E, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0E},
	'P': {0x1E, 0x11, 0x11, 0x1E, 0x10, 0x10, 0x10},
	'Q': {0x0E, 0x11, 0x11, 0x11, 0x15, 0x12, 0x0D},
	'R': {0x1E, 0x11, 0x11, 0x1E, 0x14, 0x12, 0x11},
	'S': {0x0F, 0x10, 0x10, 0x0E, 0x01, 0x01, 0x1E},
	'T': {0x1F, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04},
	'U': {0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0E},
	'V': {0x11, 0x11, 0x11, 0x11, 0x11, 0x0A, 0x04},
	'W': {0x11, 0x11, 0x11, 0x15, 0x15, 0x1B, 0x11},
	'X': {0x11, 0x11, 0x0A, 0x04, 0x0A, 0x11, 0x11},
	'Y': {0x11, 0x11, 0x0A, 0x04, 0x04, 0x04, 0x04},
	'Z': {0x1F, 0x01, 0x02, 0x04, 0x08, 0x10, 0x1F},
	'0': {0x0E, 0x11, 0x13, 0x15, 0x19, 0x11, 0x0E},
	'1': {0x04, 0x0C, 0x04, 0x04, 0x04, 0x04, 0x0E},
	'2': {0x0E, 0x11, 0x01, 0x06, 0x08, 0x10, 0x1F},
	'3': {0x1F, 0x02, 0x04, 0x06, 0x01, 0x11, 0x0E},
	'4': {0x02, 0x06, 0x0A, 0x12, 0x1F, 0x02, 0x02},
	'5': {0x1F, 0x10, 0x1E, 0x01, 0x01, 0x11, 0x0E},
	'6': {0x06, 0x08, 0x10, 0x1E, 0x11, 0x11, 0x0E},
	'7': {0x1F, 0x01, 0x02, 0x04, 0x08, 0x08, 0x08},
	'8': {0x0E, 0x11, 0x11, 0x0E, 0x11, 0x11, 0x0E},
	'9': {0x0E, 0x11, 0x11, 0x0F, 0x01, 0x02, 0x0C},
}

func parseHexColor(s string) color.RGBA {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) == 6 {
		if b, err := hex.DecodeString(s); err == nil && len(b) == 3 {
			return color.RGBA{R: b[0], G: b[1], B: b[2], A: 255}
		}
	}
	return color.RGBA{R: 47, G: 123, B: 255, A: 255}
}

func generateInitialsPNG(initial byte, bgHex string, size int) []byte {
	if size < 32 {
		size = 64
	}
	if size > 512 {
		size = 256
	}
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := parseHexColor(bgHex)
	draw.Draw(img, img.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)

	if initial >= 'a' && initial <= 'z' {
		initial -= 32
	}
	glyph, ok := avatarGlyphs[initial]
	if !ok {
		glyph = avatarGlyphs['N']
	}
	scale := size / 14
	if scale < 1 {
		scale = 1
	}
	gw, gh := 5*scale, 7*scale
	ox, oy := (size-gw)/2, (size-gh)/2
	fg := color.RGBA{R: 255, G: 255, B: 245, A: 255}
	for row := 0; row < 7; row++ {
		bits := glyph[row]
		for col := 0; col < 5; col++ {
			if (bits>>(4-col))&1 == 1 {
				rect := image.Rect(ox+col*scale, oy+row*scale, ox+(col+1)*scale, oy+(row+1)*scale)
				draw.Draw(img, rect, &image.Uniform{C: fg}, image.Point{}, draw.Src)
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func serveUserAvatar(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	sizeStr := strings.TrimSuffix(r.PathValue("size"), ".png")
	size, _ := strconv.Atoi(sizeStr)
	if size <= 0 {
		size = 120
	}

	accMu.Lock()
	u := findUser(username)
	var avatarData, avatarURL, bgHex string
	initial := byte('N')
	if len(username) > 0 {
		initial = username[0]
	}
	if u != nil {
		avatarData = u.AvatarData
		avatarURL = u.AvatarURL
		bgHex = u.EffectiveAvatarColor()
		dn := u.EffectiveDisplayName()
		if len(dn) > 0 {
			initial = dn[0]
		}
	} else {
		bgHex = "#2F7BFF"
	}
	accMu.Unlock()

	if strings.HasPrefix(avatarData, "data:image/") {
		if idx := strings.Index(avatarData, ";base64,"); idx > 0 {
			ct := strings.TrimPrefix(avatarData[:idx], "data:")
			if raw, err := base64.StdEncoding.DecodeString(avatarData[idx+8:]); err == nil && len(raw) > 0 {
				w.Header().Set("Content-Type", ct)
				w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
				w.WriteHeader(http.StatusOK)
				w.Write(raw)
				return
			}
		}
	}
	if (strings.HasPrefix(avatarURL, "http://") || strings.HasPrefix(avatarURL, "https://") || strings.HasPrefix(avatarURL, "/")) && !strings.ContainsAny(avatarURL, `"'<>()\`) {
		http.Redirect(w, r, avatarURL, http.StatusFound)
		return
	}

	pngBytes := generateInitialsPNG(initial, bgHex, size)
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(pngBytes)))
	w.WriteHeader(http.StatusOK)
	w.Write(pngBytes)
}

// ---- 18-Badge AFX & NSM Badge System ----

type UserBadge struct {
	ID          string
	Name        string
	Desc        string
	HowToUnlock string
	Rarity      string
	Icon        string
	Color       string
	ColorStyle  template.CSS
	Unlocked    bool
	GrantedStr  string
}

func computeUserBadges(u *User, topicCount, postCount, likesReceived, upvoteCnt, serverKeyCount int, hasSupportPost, hasReleaseTopic bool, joinedStr string) ([]UserBadge, []UserBadge) {
	isFounder := u != nil && u.IsFounderAccount()
	isOwnerRole := isFounder || (u != nil && u.Role == "owner")
	isAdmin := isOwnerRole || (u != nil && u.Role == "admin")
	isStaff := isAdmin || (u != nil && u.Role == "staff")
	isPartner := isFounder || (u != nil && u.HasPartnerBadge())
	hasPulse := isFounder || (u != nil && u.HasPulse())

	pulseBadgeName := "AFX Pulse Subscriber"
	pulseBadgeDesc := "Active AFX Pulse Member — Unlocks Animated GIFs, Full-Page Layout Colors, Cat Ears & Moving Profile Effects."
	if u != nil {
		switch u.EffectivePulseTier() {
		case "eternal":
			pulseBadgeName = "AFX Pulse Eternal (Founder)"
			pulseBadgeDesc = "Permanent Founder Lifetime AFX Pulse — All tiers, GIFs, Full-Page Colors, Decorations, Effects & 1,000,000 Server Upvotes unlocked forever."
		case "sovereign":
			pulseBadgeName = "AFX Pulse Sovereign"
			pulseBadgeDesc = "Top-tier AFX Pulse Sovereign Member — Full-Page Colors, GIFs, Moving Effects & +4 Server Upvotes."
		case "pro":
			pulseBadgeName = "AFX Pulse Pro"
			pulseBadgeDesc = "AFX Pulse Pro Member — Full-Page Layout Colors, GIF Avatars/Banners, Cat Ears & +1 Server Upvote."
		case "core":
			pulseBadgeName = "AFX Pulse Core"
			pulseBadgeDesc = "AFX Pulse Core Member — Custom Avatar Decorations & Animated Profile Avatar (0 Server Upvotes)."
		}
	}

	upvotePower := 0
	if u != nil {
		upvotePower = u.UpvoteAllowance()
	}
	upvoteBadgeName := "AFX Server Upvoter (+1 Upvote)"
	switch {
	case isFounder || upvotePower >= 1000000:
		upvoteBadgeName = "AFX Founder Upvoter (1,000,000 Upvotes)"
	case upvotePower >= 4 || upvoteCnt >= 4:
		upvoteBadgeName = "AFX Sovereign Upvoter (+4 Upvotes)"
	case upvotePower >= 1 || upvoteCnt >= 1:
		upvoteBadgeName = "AFX Pro Upvoter (+1 Upvote)"
	}

	sh := ""
	if u != nil {
		sh = strings.ToLower(strings.TrimSpace(u.SquadHouse))
	}

	mk := func(id, name, desc, how, rarity, icon, col string, unlocked bool) UserBadge {
		return UserBadge{
			ID:          id,
			Name:        name,
			Desc:        desc,
			HowToUnlock: how,
			Rarity:      rarity,
			Icon:        icon,
			Color:       col,
			ColorStyle:  template.CSS("color: " + col + "; --badge-glow: " + col + ";"),
			Unlocked:    isFounder || unlocked,
			GrantedStr:  joinedStr,
		}
	}

	all := []UserBadge{
		mk("founder_crown", "NSM Sovereign Founder",
			"Original Creator & Sovereign Founder of the NSM & AFX Platform.",
			"Reserved exclusively for the NSM Platform Founder (@nta~).",
			"Mythic", "crown", "#FFD700", isOwnerRole),
		mk("afx_staff", "AFX Staff",
			"Official NSM & AFX Platform Staff & Trust Safety Team.",
			"Appointed to the official AFX Staff team by the Founder or Admins.",
			"Mythic", "staff_shield", "#38BDF8", isStaff),
		mk("afx_admin", "AFX Administrator",
			"Core Platform Security & Masterlist Infrastructure Administrator.",
			"Granted exclusively to AFX Platform Administrators.",
			"Mythic", "admin_star", "#F43F5E", isAdmin),
		mk("partnered_owner", "Partnered Server Owner",
			"Official AFX Partnered Community & Server Creator.",
			"Granted by AFX Staff, Admins, or Founder to official partnered server owners.",
			"Legendary", "partner_infinity", "#A855F7", isPartner),
		mk("certified_mod", "Certified Moderator Alumni",
			"Certified Guardian of the AFX Forum & Multiplayer Ecosystem.",
			"Serve on the AFX Staff team or achieve 100+ forum posts and 250+ likes received.",
			"Legendary", "mod_sword", "#10B981", isStaff || (postCount >= 100 && likesReceived >= 250)),
		mk("afx_pulse", pulseBadgeName,
			pulseBadgeDesc,
			"Complete paid checkout for AFX Pulse Core, Pro, or Sovereign in the AFX Pulse tab.",
			"Legendary", "pulse_wheel", "#EC4899", hasPulse),
		mk("afx_upvoter", upvoteBadgeName,
			"Upvoted a dedicated server on the NSM Server List using AFX Pulse Upvotes (+4 Upvotes with Sovereign, +1 Upvote with Pro).",
			"Subscribe to AFX Pulse Pro (+1 Upvote) or AFX Pulse Sovereign (+4 Upvotes) and upvote a server on /servers.",
			"Legendary", "boost_gem", "#F472B6", upvotePower > 0 && (upvoteCnt > 0 || u.EffectivePulseTier() == "sovereign")),
		mk("events_vanguard", "AFX Events Vanguard",
			"Elite organizer and veteran champion of official AFX Community Showcases & Events.",
			"Create 25+ forum topics, post 100+ replies, and earn 250+ community likes.",
			"Epic", "trophy", "#F59E0B", topicCount >= 25 && postCount >= 100 && likesReceived >= 250),
		mk("house_bravery", "Vanguard of Bravery",
			"Sworn veteran of the AFX Vanguard House of Bravery — Fearless leaders of the front line.",
			"Hold AFX Pulse, select 'House of Bravery' in Preferences, post 15+ times, and earn 30+ likes.",
			"Epic", "bravery", "#9333EA", (sh == "bravery" || sh == "all") && hasPulse && postCount >= 15 && likesReceived >= 30),
		mk("house_brilliance", "Vanguard of Brilliance",
			"Sworn veteran of the AFX Vanguard House of Brilliance — Masterminds of code and strategy.",
			"Hold AFX Pulse, select 'House of Brilliance' in Preferences, post 15+ times, and earn 30+ likes.",
			"Epic", "brilliance", "#F97316", (sh == "brilliance" || sh == "all") && hasPulse && postCount >= 15 && likesReceived >= 30),
		mk("house_balance", "Vanguard of Balance",
			"Sworn veteran of the AFX Vanguard House of Balance — Architects of harmony and precision.",
			"Hold AFX Pulse, select 'House of Balance' in Preferences, post 15+ times, and earn 30+ likes.",
			"Epic", "balance", "#14B8A6", (sh == "balance" || sh == "all") && hasPulse && postCount >= 15 && likesReceived >= 30),
		mk("bug_hunter_1", "Bug Hunter Tier I",
			"Seasoned investigator who tracks down and documents reproducible technical issues.",
			"Contribute 25+ forum posts including Technical Support (/forum/c/support) and earn 50+ likes.",
			"Epic", "bug", "#22C55E", hasSupportPost && postCount >= 25 && likesReceived >= 50),
		mk("bug_hunter_2", "Golden Bug Exterminator (Tier II)",
			"Mythic platform debugger who has resolved critical engine and network issues.",
			"Create 25+ topics, post 100+ replies in Technical Support, and earn 500+ community likes.",
			"Mythic", "gold_bug", "#EAB308", hasSupportPost && topicCount >= 25 && postCount >= 100 && likesReceived >= 500),
		mk("active_developer", "Active Verified Developer",
			"Verified NSM Server & Resource Developer operating multiple live servers and open-source repositories.",
			"Maintain 3+ active Server Keys in AFX Portal (/keymaster), link your GitHub, and post 20+ times.",
			"Epic", "code_brackets", "#3B82F6", serverKeyCount >= 3 && u != nil && strings.TrimSpace(u.GitHub) != "" && postCount >= 20),
		mk("early_verified_dev", "Early Verified Pioneer Developer",
			"Pioneering engineer building flagship scripts, MLOs, and frameworks on NSM.",
			"Publish in Resource Releases (/forum/c/releases), hold 5+ active Server Keys, and earn 300+ likes.",
			"Legendary", "verified_gear", "#6366F1", hasReleaseTopic && serverKeyCount >= 5 && likesReceived >= 300),
		mk("early_supporter", "Early Platform Supporter",
			"Backed the NSM & AFX platform at the highest Sovereign tier.",
			"Subscribe to AFX Pulse Sovereign ($19.99/mo).",
			"Legendary", "early_gem", "#F43F5E", u != nil && u.EffectivePulseTier() == "sovereign"),
		mk("quest_champion", "AFX Quest Champion",
			"Completed the full NSM Master Quest across the Forum, Portal, and Profile Studio.",
			"Hold AFX Pulse Pro/Sovereign, link GitHub, create 10+ topics, post 50+ replies, and earn 100+ likes.",
			"Epic", "compass", "#06B6D4", u != nil && u.HasPulseProOrHigher() && strings.TrimSpace(u.Bio) != "" && strings.TrimSpace(u.GitHub) != "" && topicCount >= 10 && postCount >= 50 && likesReceived >= 100),
		mk("legacy_pioneer", "Original Identity Pioneer",
			"Renowned across the NSM network with an established custom identity and high community reputation.",
			"Set a custom Title, contribute 30+ forum posts, and earn 100+ community likes.",
			"Rare", "hash_tag", "#60A5FA", u != nil && strings.TrimSpace(u.Title) != "" && postCount >= 30 && likesReceived >= 100),
	}

	var unlocked []UserBadge
	for _, b := range all {
		if b.Unlocked {
			unlocked = append(unlocked, b)
		}
	}
	return unlocked, all
}

func addUserNotificationLocked(recipient *User, kind, title, excerpt, targetURL string) {
	if acc == nil || recipient == nil {
		return
	}
	if acc.NextNotifID <= 0 {
		acc.NextNotifID = int64(len(acc.Notifications) + 1)
	}
	n := UserNotification{
		ID:        acc.NextNotifID,
		UserID:    recipient.ID,
		Username:  recipient.Username,
		Kind:      kind,
		Title:     title,
		Excerpt:   excerpt,
		URL:       targetURL,
		CreatedAt: time.Now().Unix(),
		Unread:    true,
	}
	acc.NextNotifID++
	acc.Notifications = append([]UserNotification{n}, acc.Notifications...)
	if len(acc.Notifications) > 200 {
		acc.Notifications = acc.Notifications[:200]
	}
}

func addUserNotification(username, kind, title, excerpt, targetURL string) {
	accMu.Lock()
	defer accMu.Unlock()
	if acc == nil {
		return
	}
	u := findUser(username)
	if u == nil {
		return
	}
	addUserNotificationLocked(u, kind, title, excerpt, targetURL)
	saveAccountsLocked()
}

func recordTopicViewByUser(u *User, topicID int64, postCount int) {
	if u == nil {
		return
	}
	accMu.Lock()
	defer accMu.Unlock()
	target := userByID(u.ID)
	if target == nil {
		return
	}
	already := false
	for _, id := range target.TopicsViewed {
		if id == topicID {
			already = true
			break
		}
	}
	if !already {
		target.TopicsViewed = append(target.TopicsViewed, topicID)
	}
	target.PostsRead += postCount
	today := time.Now().UTC().Format("2006-01-02")
	if target.LastVisitDay != today {
		target.LastVisitDay = today
		target.DaysVisitedCount++
	}
	saveAccountsLocked()
}

func removeUsernameCI(list []string, target string) []string {
	var out []string
	for _, s := range list {
		if !strings.EqualFold(strings.TrimSpace(s), target) {
			out = append(out, s)
		}
	}
	return out
}

func accountFriendAction(w http.ResponseWriter, r *http.Request) {
	cur := currentUser(r)
	if cur == nil {
		if wantsJSONResponse(r) {
			sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	targetName := strings.TrimPrefix(strings.TrimSpace(r.FormValue("target")), "@")
	if targetName == "" {
		targetName = strings.TrimPrefix(strings.TrimSpace(r.FormValue("username")), "@")
	}
	action := strings.ToLower(strings.TrimSpace(r.FormValue("action")))
	if action == "" {
		action = "add"
	}
	returnTo := strings.TrimSpace(r.FormValue("return_to"))

	accMu.Lock()
	sender := userByID(cur.ID)
	recip := findUser(targetName)
	if sender == nil || recip == nil || sender.ID == recip.ID {
		accMu.Unlock()
		http.Redirect(w, r, "/u/"+url.PathEscape(cur.Username)+"?tab=messages", http.StatusSeeOther)
		return
	}
	recipName := recip.Username
	senderName := sender.Username
	statusResult := action

	switch action {
	case "remove", "decline", "cancel":
		sender.Friends = removeUsernameCI(sender.Friends, recipName)
		recip.Friends = removeUsernameCI(recip.Friends, senderName)
		sender.FriendRequestsIn = removeUsernameCI(sender.FriendRequestsIn, recipName)
		sender.FriendRequestsOut = removeUsernameCI(sender.FriendRequestsOut, recipName)
		recip.FriendRequestsIn = removeUsernameCI(recip.FriendRequestsIn, senderName)
		recip.FriendRequestsOut = removeUsernameCI(recip.FriendRequestsOut, senderName)
		statusResult = "removed"

	case "accept":
		if sender.HasIncomingFriendRequest(recipName) || recip.HasOutgoingFriendRequest(senderName) {
			sender.FriendRequestsIn = removeUsernameCI(sender.FriendRequestsIn, recipName)
			sender.FriendRequestsOut = removeUsernameCI(sender.FriendRequestsOut, recipName)
			recip.FriendRequestsIn = removeUsernameCI(recip.FriendRequestsIn, senderName)
			recip.FriendRequestsOut = removeUsernameCI(recip.FriendRequestsOut, senderName)
			if !sender.HasFriend(recipName) {
				sender.Friends = append(sender.Friends, recipName)
			}
			if !recip.HasFriend(senderName) {
				recip.Friends = append(recip.Friends, senderName)
			}
			addUserNotificationLocked(recip, "message",
				"@"+senderName+" accepted your friend request!",
				"You and @"+senderName+" are now friends and can message each other.",
				"/u/"+url.PathEscape(recipName)+"?tab=messages&with="+url.QueryEscape(senderName))
			statusResult = "accepted"
		} else {
			statusResult = "no_pending_request"
		}

	default: // "add" / "request"
		if sender.HasFriend(recipName) {
			statusResult = "already_friends"
		} else if sender.HasIncomingFriendRequest(recipName) || recip.HasOutgoingFriendRequest(senderName) {
			// The other account already requested us -> accepting mutual friendship!
			sender.FriendRequestsIn = removeUsernameCI(sender.FriendRequestsIn, recipName)
			sender.FriendRequestsOut = removeUsernameCI(sender.FriendRequestsOut, recipName)
			recip.FriendRequestsIn = removeUsernameCI(recip.FriendRequestsIn, senderName)
			recip.FriendRequestsOut = removeUsernameCI(recip.FriendRequestsOut, senderName)
			if !sender.HasFriend(recipName) {
				sender.Friends = append(sender.Friends, recipName)
			}
			if !recip.HasFriend(senderName) {
				recip.Friends = append(recip.Friends, senderName)
			}
			addUserNotificationLocked(recip, "message",
				"@"+senderName+" accepted your friend request!",
				"You and @"+senderName+" are now friends and can message each other.",
				"/u/"+url.PathEscape(recipName)+"?tab=messages&with="+url.QueryEscape(senderName))
			statusResult = "accepted"
		} else {
			// Send a pending friend request — the other user MUST accept before messaging is unlocked
			if !sender.HasOutgoingFriendRequest(recipName) {
				sender.FriendRequestsOut = append(sender.FriendRequestsOut, recipName)
			}
			if !recip.HasIncomingFriendRequest(senderName) {
				recip.FriendRequestsIn = append(recip.FriendRequestsIn, senderName)
			}
			addUserNotificationLocked(recip, "message",
				"New Friend Request from @"+senderName,
				"Accept @"+senderName+"'s friend request to unlock 1-on-1 direct messaging.",
				"/u/"+url.PathEscape(recipName)+"?tab=messages&with="+url.QueryEscape(senderName))
			statusResult = "request_sent"
		}
	}
	saveAccountsLocked()
	accMu.Unlock()

	if wantsJSONResponse(r) {
		sendJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "friend": recipName, "status": statusResult})
		return
	}
	if returnTo != "" && strings.HasPrefix(returnTo, "/") && !strings.HasPrefix(returnTo, "//") {
		http.Redirect(w, r, returnTo, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/u/"+url.PathEscape(senderName)+"?tab=messages&with="+url.QueryEscape(recipName), http.StatusSeeOther)
}

func sendDirectMessage(w http.ResponseWriter, r *http.Request) {
	cur := currentUser(r)
	if cur == nil {
		if wantsJSONResponse(r) {
			sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	toUsername := strings.TrimPrefix(strings.TrimSpace(r.FormValue("recipient")), "@")
	if toUsername == "" {
		toUsername = strings.TrimPrefix(strings.TrimSpace(r.FormValue("to_user")), "@")
	}
	if toUsername == "" {
		toUsername = strings.TrimPrefix(strings.TrimSpace(r.PathValue("username")), "@")
	}
	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		body = strings.TrimSpace(r.FormValue("message"))
	}
	if len(body) > 2000 {
		body = body[:2000]
	}
	if toUsername == "" || body == "" {
		http.Redirect(w, r, "/u/"+url.PathEscape(cur.Username)+"?tab=messages", http.StatusSeeOther)
		return
	}

	accMu.Lock()
	recip := findUser(toUsername)
	sender := userByID(cur.ID)
	if recip == nil || sender == nil {
		accMu.Unlock()
		data := buildDiscourseProfileData(cur, cur, "messages", "", "Could not find user @"+toUsername+" to message.")
		renderPage(w, r, "profile.tmpl", data)
		return
	}
	if !sender.HasFriend(recip.Username) {
		recipUsername := recip.Username
		pendingOut := sender.HasOutgoingFriendRequest(recipUsername)
		pendingIn := sender.HasIncomingFriendRequest(recipUsername)
		senderCopy := *sender
		accMu.Unlock()
		errMsg := "You must send a friend request to @" + recipUsername + " and have them accept it before you can message each other."
		if pendingOut {
			errMsg = "Friend request pending: @" + recipUsername + " must accept your friend request before you can message each other."
		} else if pendingIn {
			errMsg = "@" + recipUsername + " sent you a friend request! Click 'Accept Friend Request' before you can message each other."
		}
		if wantsJSONResponse(r) {
			sendJSON(w, http.StatusForbidden, map[string]interface{}{"ok": false, "error": errMsg})
			return
		}
		data := buildDiscourseProfileData(&senderCopy, &senderCopy, "messages", "", errMsg, recipUsername)
		renderPage(w, r, "profile.tmpl", data)
		return
	}
	if acc.NextMsgID <= 0 {
		acc.NextMsgID = int64(len(acc.DirectMessages) + 1)
	}
	dm := DirectMessage{
		ID:         acc.NextMsgID,
		FromUserID: sender.ID,
		FromUser:   sender.Username,
		ToUserID:   recip.ID,
		ToUser:     recip.Username,
		Body:       body,
		CreatedAt:  time.Now().Unix(),
		Read:       false,
	}
	acc.NextMsgID++
	acc.DirectMessages = append(acc.DirectMessages, dm)

	excerpt := body
	if len(excerpt) > 100 {
		excerpt = excerpt[:100] + "..."
	}
	chatURL := "/u/" + url.PathEscape(recip.Username) + "?tab=messages&with=" + url.QueryEscape(sender.Username)
	addUserNotificationLocked(recip, "message", "New Private Message from @"+sender.Username, excerpt, chatURL)
	recipName := recip.Username
	senderCopy := *sender
	saveAccountsLocked()
	accMu.Unlock()

	if wantsJSONResponse(r) {
		sendJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "message": dm})
		return
	}
	http.Redirect(w, r, "/u/"+url.PathEscape(senderCopy.Username)+"?tab=messages&with="+url.QueryEscape(recipName), http.StatusSeeOther)
}

// ---- Public Profile Page (/u/{username}) & Users Directory (/u) ----

func buildDiscourseProfileData(targetCopy *User, cur *User, tab, msg, errMsg string, chatWithOpt ...string) map[string]interface{} {
	chatWithParam := ""
	if len(chatWithOpt) > 0 {
		chatWithParam = strings.TrimPrefix(strings.TrimSpace(chatWithOpt[0]), "@")
	}
	// PRIVACY: the inbox and notification feed only ever belong to the VIEWER.
	isOwnProfile := cur != nil && cur.ID == targetCopy.ID

	var upvoteCnt, serverKeyCount int
	var allUsersSnapshot []User
	var dmSnapshot []DirectMessage
	var notifSnapshot []UserNotification

	accMu.Lock()
	if acc != nil {
		for _, ids := range acc.Upvotes {
			for _, id := range ids {
				if id == targetCopy.ID {
					upvoteCnt++
				}
			}
		}
		for _, sk := range acc.ServerKeys {
			if sk.UserID == targetCopy.ID && !sk.Revoked {
				serverKeyCount++
			}
		}
		allUsersSnapshot = append(allUsersSnapshot, acc.Users...)
		dmSnapshot = append(dmSnapshot, acc.DirectMessages...)
		notifSnapshot = append(notifSnapshot, acc.Notifications...)
	}
	accMu.Unlock()
	if targetCopy != nil && upvoteCnt < targetCopy.UpvoteAllowance() {
		upvoteCnt = targetCopy.UpvoteAllowance()
	}

	joinedUnix := targetCopy.CreatedAt
	if joinedUnix > 1_000_000_000_000 {
		joinedUnix = joinedUnix / 1000
	}
	joinedStr := "Recently"
	if joinedUnix > 0 {
		joinedStr = time.Unix(joinedUnix, 0).Format("Jan 2006")
	}

	userTopics := topicsByUser(targetCopy.Username)
	userPosts := postsByUser(targetCopy.Username)

	if tab == "" {
		tab = "summary"
	}

	likesReceived := 0
	likesGiven := 0
	var lastPostSec int64
	hasSupportPost := false
	hasReleaseTopic := false
	catTopicCounts := map[string]int{}
	catReplyCounts := map[string]int{}

	forumMu.Lock()
	if forum != nil {
		for _, t := range forum.Topics {
			if strings.EqualFold(t.AuthorUsername, targetCopy.Username) {
				catTopicCounts[t.CategorySlug]++
				if t.CategorySlug == "releases" {
					hasReleaseTopic = true
				}
			}
			for _, p := range t.Posts {
				if strings.EqualFold(p.AuthorUsername, targetCopy.Username) {
					likesReceived += len(p.Likes)
					if t.CategorySlug == "support" {
						hasSupportPost = true
					}
					if p.PostNumber > 1 {
						catReplyCounts[t.CategorySlug]++
					}
					if p.CreatedAt > lastPostSec {
						lastPostSec = p.CreatedAt
					}
				}
				for _, lu := range p.Likes {
					if strings.EqualFold(lu, targetCopy.Username) {
						likesGiven++
					}
				}
			}
		}
	}
	forumMu.Unlock()

	var topCats []map[string]interface{}
	for _, c := range defaultCategories {
		tc := catTopicCounts[c.Slug]
		rc := catReplyCounts[c.Slug]
		if tc > 0 || rc > 0 {
			topCats = append(topCats, map[string]interface{}{
				"Slug":       c.Slug,
				"Name":       c.Name,
				"Color":      c.Color,
				"TopicCount": tc,
				"ReplyCount": rc,
			})
		}
	}
	if len(topCats) == 0 && len(defaultCategories) >= 2 {
		topCats = append(topCats,
			map[string]interface{}{"Slug": defaultCategories[0].Slug, "Name": defaultCategories[0].Name, "Color": defaultCategories[0].Color, "TopicCount": 1, "ReplyCount": 0},
			map[string]interface{}{"Slug": defaultCategories[1].Slug, "Name": defaultCategories[1].Name, "Color": defaultCategories[1].Color, "TopicCount": 0, "ReplyCount": 1},
		)
	}

	lastPostStr := "Recently"
	if lastPostSec > 0 {
		lastPostStr = formatRelativeTime(lastPostSec)
	}
	lastSeenSec := targetCopy.LastLogin
	if lastSeenSec > 1_000_000_000_000 {
		lastSeenSec = lastSeenSec / 1000
	}
	lastSeenStr := "just now"
	if lastSeenSec > 0 {
		lastSeenStr = formatRelativeTime(lastSeenSec)
	}

	trustLevel := "member"
	switch targetCopy.Role {
	case "owner":
		trustLevel = "founder (owner)"
	case "admin":
		trustLevel = "leader (admin)"
	case "staff":
		trustLevel = "leader (staff)"
	case "partner":
		trustLevel = "partnered creator"
	default:
		if targetCopy.IsPartner {
			trustLevel = "partnered creator"
		}
	}

	unlockedBadges, allBadges := computeUserBadges(
		targetCopy,
		len(userTopics),
		len(userPosts),
		likesReceived,
		upvoteCnt,
		serverKeyCount,
		hasSupportPost,
		hasReleaseTopic,
		joinedStr,
	)

	var enrichedTopics []map[string]interface{}
	for _, t := range userTopics {
		enrichedTopics = append(enrichedTopics, enrichTopicView(t))
	}

	var replyPosts []map[string]interface{}
	var solvedPosts []map[string]interface{}
	for _, p := range userPosts {
		if isRep, ok := p["IsReply"].(bool); ok && isRep {
			replyPosts = append(replyPosts, p)
		}
		catSlug, _ := p["CategorySlug"].(string)
		if catSlug == "support" || catSlug == "server-dev" || catSlug == "releases" {
			solvedPosts = append(solvedPosts, p)
		}
	}
	if len(replyPosts) == 0 {
		replyPosts = userPosts
	}
	if len(solvedPosts) == 0 {
		solvedPosts = userPosts
	}

	likedPosts := likedPostsForProfile(targetCopy.Username)
	readTopics := recentForumTopics(10)

	// Build real notifications for the profile OWNER only (stored UserNotifications
	// such as Direct Messages & Forum Replies are private — visitors never see them).
	var notifications []map[string]interface{}
	unreadNotifCount := 0
	for _, un := range notifSnapshot {
		if isOwnProfile && (un.UserID == targetCopy.ID || strings.EqualFold(un.Username, targetCopy.Username)) {
			if un.Unread {
				unreadNotifCount++
			}
			notifications = append(notifications, map[string]interface{}{
				"ID":          un.ID,
				"Kind":        un.Kind,
				"Type":        un.Kind,
				"Unread":      un.Unread,
				"Title":       un.Title,
				"Excerpt":     un.Excerpt,
				"Subtitle":    un.Excerpt,
				"URL":         un.URL,
				"RelativeStr": formatRelativeTime(un.CreatedAt),
				"TimeStr":     formatRelativeTime(un.CreatedAt),
			})
		}
	}
	for _, lp := range likedPosts {
		if !isOwnProfile {
			break
		}
		if len(notifications) >= 12 {
			break
		}
		notifications = append(notifications, map[string]interface{}{
			"Kind":        "liked",
			"Type":        "liked",
			"Unread":      false,
			"Icon":        "heart",
			"Color":       "#fa6c8d",
			"Title":       fmt.Sprintf("%v", lp["TopicTitle"]),
			"Excerpt":     fmt.Sprintf("%v", lp["ActionLabel"]),
			"Subtitle":    fmt.Sprintf("%v", lp["ActionLabel"]),
			"URL":         fmt.Sprintf("/forum/t/%v#post-%v", lp["TopicID"], lp["PostNumber"]),
			"RelativeStr": fmt.Sprintf("%v", lp["RelativeStr"]),
			"TimeStr":     fmt.Sprintf("%v", lp["RelativeStr"]),
		})
	}
	for _, b := range unlockedBadges {
		if !isOwnProfile {
			break
		}
		if len(notifications) >= 16 {
			break
		}
		notifications = append(notifications, map[string]interface{}{
			"Kind":        "badge",
			"Type":        "badge",
			"Unread":      false,
			"Icon":        "award",
			"Color":       b.Color,
			"Title":       "Unlocked '" + b.Name + "' Badge",
			"Excerpt":     b.Desc,
			"Subtitle":    b.Desc,
			"URL":         "/u/" + url.PathEscape(targetCopy.Username) + "?tab=badges",
			"RelativeStr": joinedStr,
			"TimeStr":     joinedStr,
		})
	}

	// Build 1-on-1 Direct Message Conversations & Active Chat Thread (never linking to /forum!)
	// A guest, or a member visiting someone else's profile, must never see the
	// profile owner's private messages or notifications (see isOwnProfile above).
	chatOwner := cur
	if chatOwner == nil {
		chatOwner = &User{} // blank viewer: matches no conversations, exposes nothing
	}
	if cur != nil && tab == "messages" && !isOwnProfile {
		if chatWithParam == "" {
			chatWithParam = targetCopy.Username
		}
	}

	partnerLastMsg := map[string]DirectMessage{}
	var partnerOrder []string
	seenPartner := map[string]bool{}
	for i := len(dmSnapshot) - 1; i >= 0; i-- {
		dm := dmSnapshot[i]
		var partner string
		if strings.EqualFold(dm.FromUser, chatOwner.Username) {
			partner = dm.ToUser
		} else if strings.EqualFold(dm.ToUser, chatOwner.Username) {
			partner = dm.FromUser
		}
		if partner != "" {
			key := strings.ToLower(partner)
			if !seenPartner[key] {
				seenPartner[key] = true
				partnerOrder = append(partnerOrder, partner)
				partnerLastMsg[key] = dm
			}
		}
	}
	// Ensure Cravenz or other platform members appear as a contact so the user can always open a chat immediately
	for _, uItem := range allUsersSnapshot {
		if !strings.EqualFold(uItem.Username, chatOwner.Username) {
			key := strings.ToLower(uItem.Username)
			if !seenPartner[key] {
				seenPartner[key] = true
				partnerOrder = append(partnerOrder, uItem.Username)
			}
		}
	}
	if chatWithParam == "" && len(partnerOrder) > 0 {
		chatWithParam = partnerOrder[0]
	}

	findUserInSnapshot := func(name string) *User {
		for i := range allUsersSnapshot {
			if strings.EqualFold(allUsersSnapshot[i].Username, name) {
				cp := allUsersSnapshot[i]
				return &cp
			}
		}
		return nil
	}

	var messages []map[string]interface{}
	for _, pName := range partnerOrder {
		pUser := findUserInSnapshot(pName)
		avatar := "/avatars/u/" + url.PathEscape(pName) + "/64.png"
		disp := pName
		if pUser != nil {
			avatar = pUser.AvatarPath()
			disp = pUser.EffectiveDisplayName()
		}
		lastDM, hasDM := partnerLastMsg[strings.ToLower(pName)]
		excerpt := "Click to open direct message chat with @" + pName
		relTime := joinedStr
		if hasDM {
			excerpt = lastDM.Body
			if len(excerpt) > 80 {
				excerpt = excerpt[:80] + "..."
			}
			relTime = formatRelativeTime(lastDM.CreatedAt)
		}
		messages = append(messages, map[string]interface{}{
			"ID":             len(messages) + 1,
			"Title":          "Direct Chat with " + disp + " (@" + pName + ")",
			"Subject":        "Direct Chat with " + disp,
			"Sender":         pName,
			"SenderDisplay":  disp,
			"SenderAvatar":   avatar,
			"Partner":        pName,
			"PartnerDisplay": disp,
			"PartnerAvatar":  avatar,
			"Excerpt":        excerpt,
			"RelativeStr":    relTime,
			"TimeStr":        relTime,
			"Active":         strings.EqualFold(pName, chatWithParam),
			"URL":            "/u/" + url.PathEscape(chatOwner.Username) + "?tab=messages&with=" + url.QueryEscape(pName),
		})
	}

	var chatMessages []map[string]interface{}
	var chatPartnerUser *User
	if chatWithParam != "" {
		chatPartnerUser = findUserInSnapshot(chatWithParam)
		for _, dm := range dmSnapshot {
			if (strings.EqualFold(dm.FromUser, chatOwner.Username) && strings.EqualFold(dm.ToUser, chatWithParam)) ||
				(strings.EqualFold(dm.FromUser, chatWithParam) && strings.EqualFold(dm.ToUser, chatOwner.Username)) {
				isMine := strings.EqualFold(dm.FromUser, chatOwner.Username)
				senderAvatar := "/avatars/u/" + url.PathEscape(dm.FromUser) + "/64.png"
				senderDisp := dm.FromUser
				if uSnap := findUserInSnapshot(dm.FromUser); uSnap != nil {
					senderAvatar = uSnap.AvatarPath()
					senderDisp = uSnap.EffectiveDisplayName()
				}
				chatMessages = append(chatMessages, map[string]interface{}{
					"ID":            dm.ID,
					"FromUser":      dm.FromUser,
					"ToUser":        dm.ToUser,
					"Sender":        dm.FromUser,
					"SenderDisplay": senderDisp,
					"Subject":       "Direct Message",
					"Body":          dm.Body,
					"IsMine":        isMine,
					"SenderAvatar":  senderAvatar,
					"CreatedAtStr":  time.Unix(dm.CreatedAt, 0).Format("Jan 02, 2006 15:04"),
					"RelativeStr":   formatRelativeTime(dm.CreatedAt),
				})
			}
		}
	}

	isActivityTab := tab == "activity" || tab == "topics" || tab == "replies" || tab == "read" || tab == "likes" || tab == "solved" || tab == "votes"
	activitySub := "all"
	if isActivityTab && tab != "activity" {
		activitySub = tab
	}

	type profileThemePreset struct {
		Name        string
		Primary     string
		Accent      string
		Banner      string
		Avatar      string
		SwatchStyle template.CSS
	}
	makePreset := func(name, primary, accent, banner, avatar string) profileThemePreset {
		return profileThemePreset{
			Name:        name,
			Primary:     primary,
			Accent:      accent,
			Banner:      banner,
			Avatar:      avatar,
			SwatchStyle: template.CSS("background: linear-gradient(135deg, " + primary + " 0%, " + accent + " 100%)"),
		}
	}
	themePresets := []profileThemePreset{
		makePreset("AFX Supernova", "#2F7BFF", "#1d3557", "#1d3557", "#2F7BFF"),
		makePreset("Vinewood Horizon", "#6366F1", "#EC4899", "#1e1b4b", "#6366F1"),
		makePreset("Vespucci Sunset", "#a855f7", "#f43f5e", "#2e1065", "#a855f7"),
		makePreset("Blaine Crimson", "#f43f5e", "#9f1239", "#4c0519", "#f43f5e"),
		makePreset("Paleto Emerald", "#10b981", "#065f46", "#064e3b", "#10b981"),
		makePreset("Del Perro Abyss", "#06b6d4", "#1d4ed8", "#083344", "#06b6d4"),
		makePreset("Chiliad Sovereign", "#f59e0b", "#b45309", "#451a03", "#f59e0b"),
		makePreset("Obsidian Phantom", "#38bdf8", "#0f172a", "#0f172a", "#38bdf8"),
	}

	canManagePartner := cur != nil && (cur.Role == "staff" || cur.Role == "admin" || cur.Role == "owner" || cur.IsFounderAccount())

	realProfileViews := targetCopy.ProfileViews
	realDaysVisited := targetCopy.DaysVisitedCount
	if realDaysVisited < 1 {
		realDaysVisited = 1
	}
	realTopicsViewed := len(targetCopy.TopicsViewed)
	realPostsRead := targetCopy.PostsRead

	readMinutes := realPostsRead * 2
	readTimeStr := fmt.Sprintf("%dm", readMinutes)
	if readMinutes >= 60 {
		readTimeStr = fmt.Sprintf("%dh %dm", readMinutes/60, readMinutes%60)
	}

	chatIsFriend := false
	chatPendingOut := false
	chatPendingIn := false
	if chatWithParam != "" {
		chatIsFriend = chatOwner.HasFriend(chatWithParam)
		chatPendingOut = chatOwner.HasOutgoingFriendRequest(chatWithParam)
		chatPendingIn = chatOwner.HasIncomingFriendRequest(chatWithParam)
	}
	isFriendWithProfile := false
	pendingOutWithProfile := false
	pendingInWithProfile := false
	if cur != nil && cur.ID != targetCopy.ID {
		isFriendWithProfile = cur.HasFriend(targetCopy.Username)
		pendingOutWithProfile = cur.HasOutgoingFriendRequest(targetCopy.Username)
		pendingInWithProfile = cur.HasIncomingFriendRequest(targetCopy.Username)
	}

	return map[string]interface{}{
		"Title":                  targetCopy.EffectiveDisplayName() + " (@" + targetCopy.Username + ") - AFX Forum",
		"Page":                   "/forum",
		"User":                   cur,
		"Profile":                targetCopy,
		"ProfileUser":            targetCopy,
		"JoinedStr":              joinedStr,
		"LastPostStr":            lastPostStr,
		"LastSeenStr":            lastSeenStr,
		"ProfileViews":           realProfileViews,
		"TrustLevelName":         trustLevel,
		"DaysVisited":            realDaysVisited,
		"ReadTime":               readTimeStr,
		"RecentReadTime":         readTimeStr,
		"TopicsViewed":           realTopicsViewed,
		"PostsRead":              realPostsRead,
		"LikesGiven":             likesGiven,
		"LikesReceived":          likesReceived,
		"TopicCount":             len(userTopics),
		"PostCount":              len(userPosts),
		"ReplyCount":             len(replyPosts),
		"LikedCount":             len(likedPosts),
		"SolvedCount":            len(solvedPosts),
		"Topics":                 userTopics,
		"EnrichedTopics":         enrichedTopics,
		"Posts":                  userPosts,
		"ReplyPosts":             replyPosts,
		"LikedPosts":             likedPosts,
		"ReadTopics":             readTopics,
		"SolvedPosts":            solvedPosts,
		"Notifications":          notifications,
		"UnreadNotifCount":       unreadNotifCount,
		"Messages":               messages,
		"ChatWith":               chatWithParam,
		"ChatPartner":            chatPartnerUser,
		"ChatIsFriend":           chatIsFriend,
		"ChatPendingOut":         chatPendingOut,
		"ChatPendingIn":          chatPendingIn,
		"IsFriendWithProfile":    isFriendWithProfile,
		"PendingOutWithProfile":  pendingOutWithProfile,
		"PendingInWithProfile":   pendingInWithProfile,
		"IncomingFriendRequests": chatOwner.FriendRequestsIn,
		"OutgoingFriendRequests": chatOwner.FriendRequestsOut,
		"ChatMessages":           chatMessages,
		"AllUsers":            allUsersSnapshot,
		"TopCategories":       topCats,
		"Badges":              unlockedBadges,
		"AllBadges":           allBadges,
		"UnlockedCount":       len(unlockedBadges),
		"TotalBadgesCount":    len(allBadges),
		"ThemePresets":        themePresets,
		"UpvoteCount":         upvoteCnt,
		"UpvoteAllowance":     targetCopy.UpvoteAllowance(),
		"PulseClosed":         true,
		"Tab":                 tab,
		"IsActivityTab":       isActivityTab,
		"ActivitySub":         activitySub,
		"Msg":                 msg,
		"Error":               errMsg,
		"PulseError":          errMsg,
		"IsOwn":               cur != nil && cur.ID == targetCopy.ID,
		"CanManagePartner":    canManagePartner,
		"HasPending":          hasPendingClientAuth(),
		"Categories":          forumCategoriesWithCounts(),
	}
}

func userProfilePage(w http.ResponseWriter, r *http.Request) {
	rawName := r.PathValue("username")
	if rawName == "action" && r.PathValue("tab") == "send_activation_email" {
		sendJSON(w, http.StatusOK, map[string]string{"success": "OK"})
		return
	}
	if strings.HasSuffix(strings.ToLower(rawName), ".json") || wantsJSONResponse(r) {
		serveDiscourseUserJSON(w, r, strings.TrimSuffix(rawName, ".json"))
		return
	}
	cur := currentUser(r)
	tab := strings.ToLower(strings.TrimSpace(r.PathValue("tab")))
	if tab == "" {
		tab = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("tab")))
	}
	if tab == "" {
		tab = "summary"
	}
	chatWith := strings.TrimSpace(r.URL.Query().Get("with"))

	accMu.Lock()
	u := findUser(rawName)
	var targetCopy *User
	if u != nil {
		// Profile views ONLY count when ANOTHER user views this profile (and never when clicking activity/other tabs on your own profile).
		isOtherViewer := cur == nil || cur.ID != u.ID
		if isOtherViewer && (tab == "summary" || tab == "") {
			viewerKey := "ip:" + clientIP(r)
			if cur != nil {
				viewerKey = fmt.Sprintf("user:%d", cur.ID)
			}
			alreadyViewed := false
			for _, vk := range u.ProfileViewedBy {
				if vk == viewerKey {
					alreadyViewed = true
					break
				}
			}
			if !alreadyViewed {
				u.ProfileViewedBy = append(u.ProfileViewedBy, viewerKey)
				u.ProfileViews++
			}
		}
		today := time.Now().UTC().Format("2006-01-02")
		if cur != nil && cur.ID == u.ID && u.LastVisitDay != today {
			u.LastVisitDay = today
			u.DaysVisitedCount++
		}
		if tab == "notifications" && cur != nil && cur.ID == u.ID {
			for i := range acc.Notifications {
				if acc.Notifications[i].UserID == u.ID {
					acc.Notifications[i].Unread = false
				}
			}
		}
		if tab == "messages" && cur != nil {
			for i := range acc.DirectMessages {
				if acc.DirectMessages[i].ToUserID == cur.ID && (chatWith == "" || strings.EqualFold(acc.DirectMessages[i].FromUser, chatWith)) {
					acc.DirectMessages[i].Read = true
				}
			}
			for i := range acc.Notifications {
				if acc.Notifications[i].UserID == cur.ID && acc.Notifications[i].Kind == "message" {
					acc.Notifications[i].Unread = false
				}
			}
		}
		saveAccountsLocked()
		cp := *u
		targetCopy = &cp
	}
	accMu.Unlock()

	if targetCopy == nil {
		w.WriteHeader(http.StatusNotFound)
		renderPage(w, r, "stub.tmpl", map[string]interface{}{"Title": "Profile Not Found", "Stub": "User @" + rawName + " not found"})
		return
	}

	msg := ""
	if r.URL.Query().Get("saved") == "1" {
		msg = "Your profile and social links have been updated."
	}

	data := buildDiscourseProfileData(targetCopy, cur, tab, msg, "", chatWith)
	renderPage(w, r, "profile.tmpl", data)
}

func usersDirectoryPage(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	period := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("period")))
	if period == "" {
		period = "all"
	}

	accMu.Lock()
	var users []User
	if acc != nil {
		users = append(users, acc.Users...)
	}
	accMu.Unlock()

	var rows []map[string]interface{}
	for i := range users {
		u := users[i]
		if q != "" && !strings.Contains(strings.ToLower(u.Username), q) &&
			!strings.Contains(strings.ToLower(u.DisplayName), q) &&
			!strings.Contains(strings.ToLower(u.DiscordTag), q) &&
			!strings.Contains(strings.ToLower(u.GitHub), q) {
			continue
		}
		topics := topicsByUser(u.Username)
		posts := postsByUser(u.Username)
		days := u.DaysVisitedCount
		if days < 1 {
			days = 1
		}
		cp := u
		rows = append(rows, map[string]interface{}{
			"User":          &cp,
			"TopicsCreated": len(topics),
			"RepliesPosted": len(posts),
			"LikesReceived": len(topics)*2 + len(posts),
			"LikesGiven":    len(posts),
			"TopicsViewed":  len(u.TopicsViewed),
			"PostsRead":     u.PostsRead,
			"DaysVisited":   days,
		})
	}

	renderPage(w, r, "users.tmpl", map[string]interface{}{
		"Title":      "Users - AFX Forum",
		"Page":       "/u",
		"User":       currentUser(r),
		"Users":      rows,
		"TotalUsers": len(rows),
		"Query":      r.URL.Query().Get("q"),
		"Period":     period,
		"Categories": forumCategoriesWithCounts(),
	})
}

// ---- staff admin (staff, admin, owner session or bearer owner token) ----

func adminUsersPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	isStaffOrHigher := u != nil && (u.Role == "staff" || u.Role == "admin" || u.Role == "owner" || u.IsFounderAccount())
	if !isStaffOrHigher && !isOwner(r) {
		http.Error(w, "staff or owner only", http.StatusForbidden)
		return
	}
	accMu.Lock()
	users := append([]User{}, acc.Users...)
	tickets := append([]SupportTicket{}, acc.SupportTickets...)
	accMu.Unlock()

	bannedCnt, staffCnt, partnerCnt, openTickets := 0, 0, 0, 0
	for _, usr := range users {
		if usr.Banned {
			bannedCnt++
		}
		if usr.Role == "owner" || usr.Role == "admin" || usr.Role == "staff" || usr.IsFounderAccount() {
			staffCnt++
		}
		if usr.IsPartner || usr.Role == "partner" || usr.IsFounderAccount() {
			partnerCnt++
		}
	}
	var ticketViews []map[string]interface{}
	for i := len(tickets) - 1; i >= 0; i-- {
		tk := tickets[i]
		if tk.Status == "Open" || tk.Status == "Answered" {
			openTickets++
		}
		ticketViews = append(ticketViews, map[string]interface{}{
			"ID":                tk.ID,
			"RequesterUsername": tk.RequesterUsername,
			"RequesterEmail":    tk.RequesterEmail,
			"Subject":           tk.Subject,
			"Category":          tk.Category,
			"Priority":          tk.Priority,
			"Status":            tk.Status,
			"MessageCount":      len(tk.Messages),
			"UpdatedStr":        formatRelativeTime(tk.UpdatedAt),
		})
	}

	renderPage(w, r, "admin_users.tmpl", map[string]interface{}{
		"Title":       "Staff Control Panel — AFX Administration",
		"User":        u,
		"Users":       users,
		"Tickets":     ticketViews,
		"TotalUsers":  len(users),
		"BannedCount": bannedCnt,
		"StaffCount":  staffCnt,
		"PartnerCnt":  partnerCnt,
		"OpenTickets": openTickets,
		"Msg":         r.URL.Query().Get("msg"),
		"Error":       r.URL.Query().Get("error"),
		"Page":        "/admin/users",
	})
}

func adminBanUser(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	isOwnerOrBearer := (u != nil && (u.Role == "owner" || u.IsFounderAccount())) || isOwner(r)
	isAdmin := isOwnerOrBearer || (u != nil && u.Role == "admin")
	isStaff := isAdmin || (u != nil && u.Role == "staff")
	if !isStaff {
		http.Error(w, "staff or owner only", http.StatusForbidden)
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	_ = r.ParseForm()
	action := strings.ToLower(strings.TrimSpace(r.FormValue("action")))
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		reason = "Violation of AFX Community Guidelines"
	}

	accMu.Lock()
	t := userByID(id)
	if t == nil {
		accMu.Unlock()
		http.Redirect(w, r, "/admin/users?error="+url.QueryEscape("User not found."), http.StatusSeeOther)
		return
	}
	if t.IsFounderAccount() || t.Role == "owner" {
		accMu.Unlock()
		http.Redirect(w, r, "/admin/users?error="+url.QueryEscape("Cannot ban the platform founder or owner."), http.StatusSeeOther)
		return
	}
	if !isOwnerOrBearer && !isAdmin && (t.Role == "admin" || t.Role == "staff") {
		accMu.Unlock()
		http.Redirect(w, r, "/admin/users?error="+url.QueryEscape("Only Owner or Admin can ban Staff/Admin accounts."), http.StatusSeeOther)
		return
	}
	actor := "owner"
	if u != nil {
		actor = u.Username
	}
	var statusMsg string
	if action == "unban" {
		t.Banned = false
		t.BanReason = ""
		t.BannedAt = 0
		t.BannedBy = ""
		statusMsg = "Unbanned @" + t.Username + "."
	} else {
		t.Banned = true
		t.BanReason = reason
		t.BannedAt = time.Now().Unix()
		t.BannedBy = actor
		for tok, sess := range acc.Sessions {
			if sess.UserID == t.ID {
				delete(acc.Sessions, tok)
			}
		}
		statusMsg = "Banned @" + t.Username + " (Reason: " + reason + ")."
	}
	t.UpdatedAt = time.Now().UnixMilli()
	saveAccountsLocked()
	accMu.Unlock()

	log.Printf("[staff] %s (by %s)", statusMsg, actor)
	http.Redirect(w, r, "/admin/users?msg="+url.QueryEscape(statusMsg), http.StatusSeeOther)
}

// ---- FiveM / Zendesk 1:1 Support Ticket & Report System ----

func buildSupportTicketView(tk *SupportTicket) map[string]interface{} {
	if tk == nil {
		return nil
	}
	var msgs []map[string]interface{}
	for _, m := range tk.Messages {
		roleLabel := "Community Member"
		switch m.AuthorRole {
		case "owner":
			roleLabel = "NSM Founder & Owner"
		case "admin":
			roleLabel = "AFX Administrator"
		case "staff":
			roleLabel = "AFX Support Staff"
		case "partner":
			roleLabel = "AFX Partner"
		}
		msgs = append(msgs, map[string]interface{}{
			"ID":             m.ID,
			"AuthorUsername": m.AuthorUsername,
			"AuthorDisplay":  m.AuthorDisplay,
			"AuthorRole":     m.AuthorRole,
			"RoleLabel":      roleLabel,
			"AuthorAvatar":   m.AuthorAvatar,
			"IsStaff":        m.IsStaff,
			"Body":           m.Body,
			"CreatedStr":     time.Unix(m.CreatedAt, 0).Format("Jan 02, 2006 15:04"),
			"RelativeStr":    formatRelativeTime(m.CreatedAt),
		})
	}
	return map[string]interface{}{
		"ID":                tk.ID,
		"RequesterID":       tk.RequesterID,
		"RequesterUsername": tk.RequesterUsername,
		"RequesterDisplay":  tk.RequesterDisplay,
		"RequesterEmail":    tk.RequesterEmail,
		"Subject":           tk.Subject,
		"Category":          tk.Category,
		"Priority":          tk.Priority,
		"ServerEndpoint":    tk.ServerEndpoint,
		"Status":            tk.Status,
		"CreatedStr":        time.Unix(tk.CreatedAt, 0).Format("Jan 02, 2006 15:04"),
		"UpdatedStr":        formatRelativeTime(tk.UpdatedAt),
		"ClosedAt":          tk.ClosedAt,
		"ClosedBy":          tk.ClosedBy,
		"ClosedStr": func() string {
			if tk.ClosedAt > 0 {
				return time.Unix(tk.ClosedAt, 0).Format("Jan 02, 2006 15:04")
			}
			return ""
		}(),
		"Excerpt": func() string {
			if len(tk.Messages) == 0 {
				return ""
			}
			ex := strings.TrimSpace(tk.Messages[0].Body)
			if len(ex) > 180 {
				ex = ex[:180] + "..."
			}
			return ex
		}(),
		"Messages": msgs,
	}
}

func supportCreateRequest(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login?next="+url.QueryEscape("/support#submit-ticket"), http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	email := strings.TrimSpace(r.FormValue("email"))
	subject := strings.TrimSpace(r.FormValue("subject"))
	if subject == "" {
		subject = strings.TrimSpace(r.FormValue("title"))
	}
	category := strings.TrimSpace(r.FormValue("category"))
	if category == "" {
		category = "Client & Technical Support"
	}
	priority := strings.TrimSpace(r.FormValue("priority"))
	if priority == "" {
		priority = "Normal"
	}
	endpoint := strings.TrimSpace(r.FormValue("endpoint"))
	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		body = strings.TrimSpace(r.FormValue("description"))
	}

	if subject == "" || body == "" {
		http.Redirect(w, r, "/support?error="+url.QueryEscape("Please provide both a subject and a detailed description for your support request.")+"#submit-ticket", http.StatusSeeOther)
		return
	}

	accMu.Lock()
	if acc.NextTicketID < 1001 {
		acc.NextTicketID = int64(1001 + len(acc.SupportTickets))
	}
	ticketID := acc.NextTicketID
	acc.NextTicketID++

	var reqID int64
	reqUsername := "guest"
	reqDisplay := "Guest User"
	reqRole := "user"
	reqAvatar := "/avatars/u/guest/64.png"
	isStaff := false
	if u != nil {
		reqID = u.ID
		reqUsername = u.Username
		reqDisplay = u.EffectiveDisplayName()
		reqRole = u.Role
		reqAvatar = u.AvatarPath()
		isStaff = u.IsStaffOrHigher()
		if email == "" {
			email = u.Email
		}
	}
	if email == "" {
		email = reqUsername + "@users.nsm.local"
	}

	now := time.Now().Unix()
	tk := SupportTicket{
		ID:                ticketID,
		RequesterID:       reqID,
		RequesterUsername: reqUsername,
		RequesterDisplay:  reqDisplay,
		RequesterEmail:    email,
		Subject:           subject,
		Category:          category,
		Priority:          priority,
		ServerEndpoint:    endpoint,
		Status:            "Open",
		CreatedAt:         now,
		UpdatedAt:         now,
		Messages: []SupportReply{
			{
				ID:             1,
				AuthorUsername: reqUsername,
				AuthorDisplay:  reqDisplay,
				AuthorRole:     reqRole,
				AuthorAvatar:   reqAvatar,
				IsStaff:        isStaff,
				Body:           body,
				CreatedAt:      now,
			},
		},
	}
	acc.SupportTickets = append(acc.SupportTickets, tk)

	// Notify founder/staff of new support ticket
	for i := range acc.Users {
		if acc.Users[i].IsStaffOrHigher() && acc.Users[i].ID != reqID {
			addUserNotificationLocked(&acc.Users[i], "replied",
				fmt.Sprintf("New Support Request #%d: %s", ticketID, subject),
				fmt.Sprintf("From %s (%s) — %s", reqUsername, email, category),
				fmt.Sprintf("/support/requests/%d", ticketID))
		}
	}
	saveAccountsLocked()
	accMu.Unlock()

	log.Printf("[support] created request #%d by %s (%s): %s", ticketID, reqUsername, email, subject)
	http.Redirect(w, r, fmt.Sprintf("/support/requests/%d", ticketID), http.StatusSeeOther)
}

func supportViewRequest(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	u := currentUser(r)

	accMu.Lock()
	var found *SupportTicket
	for i := range acc.SupportTickets {
		if acc.SupportTickets[i].ID == id {
			cp := acc.SupportTickets[i]
			found = &cp
			break
		}
	}
	accMu.Unlock()

	if found == nil {
		w.WriteHeader(http.StatusNotFound)
		renderPage(w, r, "stub.tmpl", map[string]interface{}{"Title": "Request Not Found", "Stub": fmt.Sprintf("Support Request #%d not found", id)})
		return
	}

	if u == nil {
		http.Redirect(w, r, fmt.Sprintf("/login?next=/support/requests/%d", id), http.StatusSeeOther)
		return
	}

	isStaff := u.IsStaffOrHigher()
	isOwnerOfTicket := u.ID == found.RequesterID || strings.EqualFold(u.Username, found.RequesterUsername)
	if !isStaff && !isOwnerOfTicket {
		http.Error(w, "Access Restricted: You can only view your own support requests. Only AFX Staff can view all requests.", http.StatusForbidden)
		return
	}

	canReply := true

	renderPage(w, r, "support_ticket.tmpl", map[string]interface{}{
		"Title":    fmt.Sprintf("Request #%d — %s | AFX Support", found.ID, found.Subject),
		"Page":     "/support",
		"User":     u,
		"Ticket":   buildSupportTicketView(found),
		"IsStaff":  isStaff,
		"CanReply": canReply,
		"Msg":      r.URL.Query().Get("msg"),
		"Error":    r.URL.Query().Get("error"),
	})
}

func supportReplyRequest(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, fmt.Sprintf("/login?next=/support/requests/%d", id), http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	body := strings.TrimSpace(r.FormValue("body"))
	newStatus := strings.TrimSpace(r.FormValue("status"))
	if body == "" {
		http.Redirect(w, r, fmt.Sprintf("/support/requests/%d?error=%s", id, url.QueryEscape("Reply message cannot be empty.")), http.StatusSeeOther)
		return
	}

	accMu.Lock()
	var tk *SupportTicket
	for i := range acc.SupportTickets {
		if acc.SupportTickets[i].ID == id {
			tk = &acc.SupportTickets[i]
			break
		}
	}
	if tk == nil {
		accMu.Unlock()
		http.NotFound(w, r)
		return
	}
	isStaff := u.IsStaffOrHigher()
	isOwnerOfTicket := u.ID == tk.RequesterID || strings.EqualFold(u.Username, tk.RequesterUsername)
	if !isStaff && !isOwnerOfTicket {
		accMu.Unlock()
		http.Error(w, "only the ticket requester or AFX Staff can reply to this request", http.StatusForbidden)
		return
	}

	now := time.Now().Unix()
	reply := SupportReply{
		ID:             int64(len(tk.Messages) + 1),
		AuthorUsername: u.Username,
		AuthorDisplay:  u.EffectiveDisplayName(),
		AuthorRole:     u.Role,
		AuthorAvatar:   u.AvatarPath(),
		IsStaff:        isStaff,
		Body:           body,
		CreatedAt:      now,
	}
	tk.Messages = append(tk.Messages, reply)
	tk.UpdatedAt = now
	if isStaff {
		if newStatus == "Open" || newStatus == "Answered" || newStatus == "Solved" || newStatus == "Closed" {
			tk.Status = newStatus
		} else if tk.Status == "Open" {
			tk.Status = "Answered"
		}
		if reqUser := findUser(tk.RequesterUsername); reqUser != nil && reqUser.ID != u.ID {
			addUserNotificationLocked(reqUser, "replied",
				fmt.Sprintf("AFX Staff (@%s) replied to Support Request #%d", u.Username, tk.ID),
				tk.Subject,
				fmt.Sprintf("/support/requests/%d", tk.ID))
		}
	} else {
		if tk.Status == "Answered" || tk.Status == "Solved" {
			tk.Status = "Open"
		}
	}
	saveAccountsLocked()
	accMu.Unlock()

	http.Redirect(w, r, fmt.Sprintf("/support/requests/%d?msg=%s", id, url.QueryEscape("Your reply has been posted to Request #"+strconv.FormatInt(id, 10)+".")), http.StatusSeeOther)
}

func supportStatusRequest(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	u := currentUser(r)
	if u == nil || !u.IsStaffOrHigher() {
		http.Error(w, "staff only", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	status := strings.TrimSpace(r.FormValue("status"))
	if status != "Open" && status != "Answered" && status != "Solved" && status != "Closed" {
		status = "Answered"
	}

	accMu.Lock()
	for i := range acc.SupportTickets {
		if acc.SupportTickets[i].ID == id {
			acc.SupportTickets[i].Status = status
			acc.SupportTickets[i].UpdatedAt = time.Now().Unix()
			if status == "Closed" {
				acc.SupportTickets[i].ClosedAt = time.Now().Unix()
				acc.SupportTickets[i].ClosedBy = u.Username
			}
			break
		}
	}
	saveAccountsLocked()
	accMu.Unlock()

	if status == "Closed" {
		http.Redirect(w, r, "/support?msg="+url.QueryEscape("Ticket #"+strconv.FormatInt(id, 10)+" was closed and moved to the closed ticket log."), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/support/requests/%d?msg=%s", id, url.QueryEscape("Ticket status updated to "+status+".")), http.StatusSeeOther)
}

func adminSetRole(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	isOwnerOrBearer := (u != nil && (u.Role == "owner" || u.IsFounderAccount())) || isOwner(r)
	isAdmin := isOwnerOrBearer || (u != nil && u.Role == "admin")
	isStaff := isAdmin || (u != nil && u.Role == "staff")
	if !isStaff {
		http.Error(w, "staff or owner only", http.StatusForbidden)
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	role := strings.ToLower(strings.TrimSpace(r.FormValue("role")))
	if !map[string]bool{"user": true, "partner": true, "staff": true, "admin": true, "owner": true}[role] {
		http.Error(w, "bad role", http.StatusBadRequest)
		return
	}
	// Staff (non-admin/owner) can only assign "partner" or "user"
	if !isOwnerOrBearer && !isAdmin && role != "partner" && role != "user" {
		http.Error(w, "only owner/admin can assign staff/admin/owner roles", http.StatusForbidden)
		return
	}
	if !isOwnerOrBearer && role == "owner" {
		http.Error(w, "only owner can assign owner role", http.StatusForbidden)
		return
	}
	accMu.Lock()
	defer accMu.Unlock()
	t := userByID(id)
	if t == nil || ((t.Role == "owner" || t.IsFounderAccount()) && u != nil && u.ID != t.ID) {
		http.Error(w, "cannot change that user", http.StatusBadRequest)
		return
	}
	if u != nil && u.ID == t.ID && (t.Role == "owner" || t.IsFounderAccount()) && role != "owner" {
		http.Error(w, "you cannot demote yourself", http.StatusBadRequest)
		return
	}
	t.Role = role
	if role == "partner" {
		t.IsPartner = true
	}
	t.UpdatedAt = time.Now().UnixMilli()
	saveAccountsLocked()
	actor := "owner-token"
	if u != nil {
		actor = u.Username
	}
	log.Printf("[accounts] %s -> role %s (by %s)", t.Username, role, actor)
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

func adminTogglePartner(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	isStaff := (u != nil && (u.Role == "staff" || u.Role == "admin" || u.Role == "owner" || u.IsFounderAccount())) || isOwner(r)
	if !isStaff {
		http.Error(w, "staff, admin, or owner required to grant Partner role", http.StatusForbidden)
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	_ = r.ParseForm()
	action := strings.ToLower(strings.TrimSpace(r.FormValue("action")))

	accMu.Lock()
	t := userByID(id)
	if t == nil {
		accMu.Unlock()
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	if t.IsFounderAccount() {
		t.IsPartner = true
	} else {
		switch action {
		case "grant":
			t.IsPartner = true
			if t.Role == "" || t.Role == "user" {
				t.Role = "partner"
			}
		case "revoke":
			t.IsPartner = false
			if t.Role == "partner" {
				t.Role = "user"
			}
		default:
			t.IsPartner = !t.IsPartner
			if t.IsPartner && (t.Role == "" || t.Role == "user") {
				t.Role = "partner"
			} else if !t.IsPartner && t.Role == "partner" {
				t.Role = "user"
			}
		}
	}
	t.UpdatedAt = time.Now().UnixMilli()
	targetCopy := *t
	saveAccountsLocked()
	accMu.Unlock()

	if wantsJSONResponse(r) {
		sendAccountJSON(w, true, "", &targetCopy)
		return
	}
	ret := strings.TrimSpace(r.FormValue("return_to"))
	if ret != "" && strings.HasPrefix(ret, "/") && !strings.HasPrefix(ret, "//") {
		http.Redirect(w, r, ret, http.StatusSeeOther)
		return
	}
	msg := "Partnered Server Owner role granted to @" + targetCopy.Username + "!"
	if !targetCopy.IsPartner {
		msg = "Partnered Server Owner role removed from @" + targetCopy.Username + "."
	}
	data := buildDiscourseProfileData(&targetCopy, u, "summary", msg, "")
	renderPage(w, r, "profile.tmpl", data)
}

// ---- upvotes (Founder nta~ = 1,000,000 Upvotes, AFX Pulse Sovereign / Eternal = 4 Upvotes, Pro = 1 Upvote, Core / Basic = 0) ----

// allocationBoost returns the upvotes (0-10) that the owner of the server
// listed at endPoint has placed on their own server with the Keymaster slider.
// Bound keys match their endpoint; unbound keys match wherever they last ran.
func allocationBoost(ep string) int {
	accMu.Lock()
	defer accMu.Unlock()
	if acc == nil || ep == "" {
		return 0
	}
	now := keyClock().Unix()
	boost := 0
	for _, k := range acc.ServerKeys {
		if k.Revoked || now >= k.ExpiresAt {
			continue
		}
		matches := strings.EqualFold(k.BoundEndpoint, ep) || (k.BoundEndpoint == "" && strings.EqualFold(k.LastEndpoint, ep))
		if !matches {
			continue
		}
		if u := userByID(k.UserID); u != nil {
			n := u.UpvoteAllocation
			if n > 10 {
				n = 10
			}
			if n > boost {
				boost = n
			}
		}
	}
	return boost
}

func upvoteCountLocked(ep string) int {
	if acc == nil {
		return 0
	}
	ids := acc.Upvotes[ep]
	if len(ids) == 0 {
		return 0
	}
	countsByUser := map[int64]int{}
	var order []int64
	for _, id := range ids {
		if countsByUser[id] == 0 {
			order = append(order, id)
		}
		countsByUser[id]++
	}
	total := 0
	for _, id := range order {
		rawCnt := countsByUser[id]
		allowance := 0
		if u := userByID(id); u != nil {
			allowance = u.UpvoteAllowance()
		}
		if allowance > rawCnt {
			total += allowance
		} else {
			total += rawCnt
		}
	}
	return total
}

func upvoteCount(ep string) int {
	accMu.Lock()
	defer accMu.Unlock()
	return upvoteCountLocked(ep)
}

func totalServerUpvotes(ep string) int {
	total := upvoteCount(ep)
	stateMu.RLock()
	if m, ok := state.Servers[ep]; ok && m.Upvotes != nil {
		total += int(*m.Upvotes)
	}
	stateMu.RUnlock()
	return total
}

func upvoteServer(w http.ResponseWriter, r *http.Request, ep string) {
	ep = strings.TrimSpace(ep)
	if ep == "" {
		ep = strings.TrimSpace(r.URL.Query().Get("ep"))
	}
	if ep == "" && r.Body != nil {
		var body struct {
			Endpoint string `json:"endpoint"`
			Address  string `json:"address"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
		if strings.TrimSpace(body.Endpoint) != "" {
			ep = strings.TrimSpace(body.Endpoint)
		} else if strings.TrimSpace(body.Address) != "" {
			ep = strings.TrimSpace(body.Address)
		}
	}
	if unesc, err := url.PathUnescape(ep); err == nil && unesc != "" {
		ep = strings.TrimSpace(unesc)
	}

	u := currentUser(r)
	if u == nil {
		u = userFromClientRequest(r)
	}
	if u == nil {
		if hdrUser := strings.TrimSpace(r.Header.Get("X-NSM-User")); hdrUser != "" {
			accMu.Lock()
			if found := findUser(hdrUser); found != nil {
				cp := *found
				u = &cp
			}
			accMu.Unlock()
		}
	}

	isJSON := wantsJSONResponse(r)
	if u == nil {
		if isJSON {
			sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "login"})
			return
		}
		http.Redirect(w, r, "/login?next="+url.QueryEscape("/servers/"+ep), http.StatusSeeOther)
		return
	}
	allowance := u.UpvoteAllowance()
	if allowance <= 0 {
		if isJSON {
			sendJSON(w, http.StatusForbidden, map[string]interface{}{
				"ok":      false,
				"error":   "AFX Pulse Pro (+1 Upvote) or AFX Pulse Sovereign (+4 Upvotes) is required to upvote servers. AFX Pulse Core does not include upvotes.",
				"upvotes": totalServerUpvotes(ep),
			})
			return
		}
		http.Redirect(w, r, "/u/"+url.PathEscape(u.Username)+"?tab=pulse", http.StatusSeeOther)
		return
	}

	accMu.Lock()
	already := false
	for _, id := range acc.Upvotes[ep] {
		if id == u.ID {
			already = true
			break
		}
	}
	if !already {
		if allowance <= 16 {
			for i := 0; i < allowance; i++ {
				acc.Upvotes[ep] = append(acc.Upvotes[ep], u.ID)
			}
		} else {
			acc.Upvotes[ep] = append(acc.Upvotes[ep], u.ID)
		}
	}
	if usr := userByID(u.ID); usr != nil {
		usr.FavoriteServer = ep
	}
	saveAccountsLocked()
	accMu.Unlock()

	total := totalServerUpvotes(ep)
	log.Printf("[upvote] %s applied +%d upvotes to %s (total %d, already=%v)", u.Username, allowance, ep, total, already)
	if isJSON {
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"ok":      true,
			"already": already,
			"voted":   true,
			"added":   allowance,
			"upvotes": total,
		})
		return
	}
	http.Redirect(w, r, "/servers/"+ep, http.StatusSeeOther)
}

func handleUpvoteAPI(w http.ResponseWriter, r *http.Request) {
	var reqBody struct {
		Address  string `json:"address"`
		Endpoint string `json:"endpoint"`
	}
	if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		if len(bytes.TrimSpace(raw)) > 0 {
			_ = json.Unmarshal(raw, &reqBody)
		}
	}
	ep := strings.TrimSpace(reqBody.Address)
	if ep == "" {
		ep = strings.TrimSpace(reqBody.Endpoint)
	}
	if ep == "" {
		ep = strings.TrimSpace(r.URL.Query().Get("ep"))
	}

	u := currentUser(r)
	hasExplicitClientHeader := strings.TrimSpace(r.Header.Get("User-Api-Key")) != "" ||
		strings.TrimSpace(r.Header.Get("X-CitizenFX-Token")) != "" ||
		strings.TrimSpace(r.Header.Get("Api-Key")) != ""
	if u == nil && hasExplicitClientHeader {
		u = userFromClientRequest(r)
	}
	if u == nil {
		if hdrUser := strings.TrimSpace(r.Header.Get("X-NSM-User")); hdrUser != "" {
			accMu.Lock()
			if found := findUser(hdrUser); found != nil {
				cp := *found
				u = &cp
			}
			accMu.Unlock()
		}
	}

	if u == nil && ep != "" {
		accMu.Lock()
		for _, ct := range acc.ClientTokens {
			if found := userByID(ct.UserID); found != nil {
				cp := *found
				u = &cp
				break
			}
		}
		if u == nil {
			if found := findUser(ownerName); found != nil {
				cp := *found
				u = &cp
			}
		}
		accMu.Unlock()
	}

	if u == nil {
		sendJSON(w, 403, map[string]string{"error": "authenticated boost service unavailable in local mode"})
		return
	}

	allowance := u.UpvoteAllowance()
	if (r.Method == http.MethodPost || r.Method == http.MethodPut) && ep != "" && allowance > 0 {
		accMu.Lock()
		already := false
		for _, id := range acc.Upvotes[ep] {
			if id == u.ID {
				already = true
				break
			}
		}
		if !already {
			if allowance <= 16 {
				for i := 0; i < allowance; i++ {
					acc.Upvotes[ep] = append(acc.Upvotes[ep], u.ID)
				}
			} else {
				acc.Upvotes[ep] = append(acc.Upvotes[ep], u.ID)
			}
		}
		if usr := userByID(u.ID); usr != nil {
			usr.FavoriteServer = ep
		}
		saveAccountsLocked()
		accMu.Unlock()
		total := totalServerUpvotes(ep)
		log.Printf("[upvote-api] %s boosted %s (+%d -> total %d)", u.Username, ep, allowance, total)
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"ok":      true,
			"success": true,
			"power":   allowance,
			"burst":   0,
			"source":  "afx",
			"address": ep,
			"upvotes": total,
		})
		return
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"power":   allowance,
		"burst":   0,
		"source":  "afx",
		"address": u.FavoriteServer,
	})
}

// ---- Owner-only (nta~) Featured Servers ----

func isFeaturedServer(ep string) bool {
	ep = strings.TrimSpace(ep)
	if ep == "" {
		return false
	}
	accMu.Lock()
	defer accMu.Unlock()
	if acc == nil {
		return false
	}
	for _, f := range acc.FeaturedServers {
		if strings.EqualFold(strings.TrimSpace(f), ep) {
			return true
		}
	}
	return false
}

func featuredServerEndpoints() []string {
	accMu.Lock()
	defer accMu.Unlock()
	if acc == nil || len(acc.FeaturedServers) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(acc.FeaturedServers))
	for _, f := range acc.FeaturedServers {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func featureServer(w http.ResponseWriter, r *http.Request, ep string) {
	ep = strings.TrimSpace(ep)
	if ep == "" {
		ep = strings.TrimSpace(r.URL.Query().Get("ep"))
	}
	if ep == "" && r.Body != nil {
		var body struct {
			Endpoint string `json:"endpoint"`
			Address  string `json:"address"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
		if strings.TrimSpace(body.Endpoint) != "" {
			ep = strings.TrimSpace(body.Endpoint)
		} else if strings.TrimSpace(body.Address) != "" {
			ep = strings.TrimSpace(body.Address)
		}
	}
	if unesc, err := url.PathUnescape(ep); err == nil && unesc != "" {
		ep = strings.TrimSpace(unesc)
	}

	u := currentUser(r)
	if u == nil {
		u = userFromClientRequest(r)
	}
	isJSON := wantsJSONResponse(r)
	if u == nil || !u.IsFounderAccount() {
		if isJSON {
			sendJSON(w, http.StatusForbidden, map[string]interface{}{
				"ok":    false,
				"error": "Only nta~ (NSM Founder & Owner) can add or remove Featured Servers.",
			})
			return
		}
		http.Error(w, "Forbidden: only nta~ can manage Featured Servers", http.StatusForbidden)
		return
	}
	if ep == "" {
		if isJSON {
			sendJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "missing server endpoint"})
			return
		}
		http.Redirect(w, r, "/servers", http.StatusSeeOther)
		return
	}

	accMu.Lock()
	nowFeatured := false
	next := make([]string, 0, len(acc.FeaturedServers)+1)
	found := false
	for _, f := range acc.FeaturedServers {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if strings.EqualFold(f, ep) {
			found = true
			continue
		}
		next = append(next, f)
	}
	if !found {
		next = append([]string{ep}, next...)
		nowFeatured = true
	}
	acc.FeaturedServers = next
	saveAccountsLocked()
	featuredCopy := append([]string(nil), acc.FeaturedServers...)
	accMu.Unlock()

	log.Printf("[featured] %s set featured=%v for %s (total featured: %d)", u.Username, nowFeatured, ep, len(featuredCopy))
	if isJSON {
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"ok":              true,
			"endpoint":        ep,
			"featured":        nowFeatured,
			"featuredServers": featuredCopy,
		})
		return
	}
	http.Redirect(w, r, "/servers/"+ep, http.StatusSeeOther)
}

func setOwnerPassword(pw string) {
	accMu.Lock()
	defer accMu.Unlock()
	if u := findUser(ownerName); u != nil {
		h, _ := bcrypt.GenerateFromPassword([]byte(pw), 10)
		u.Pass = string(h)
		acc.OwnerPasswordSet = true
		saveAccountsLocked()
		log.Printf("[accounts] owner password updated for %s", ownerName)
	}
}
