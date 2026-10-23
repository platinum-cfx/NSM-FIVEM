// NSM Join Codes — opaque, shareable connect codes (like cfx.re/join/<code>)
// that let server owners share their server WITHOUT exposing its IP address.
//
// Every server key gets its own join code ("changed per key"): issuing a new
// key mints a new code, and revoking a key kills its code. Keyless servers
// (the free-2048-slots tier) get a stable auto-assigned code per endpoint so
// every listed server can hide its IP too. Codes follow the server's live
// endpoint via heartbeats, so dynamic IPs keep working through the same link.
//
// Storage: acc.JoinCodes (persisted in accounts.json next to the exe).
package main

import (
	"crypto/rand"
	"strings"
	"time"
)

const (
	joinCodeLen      = 7
	joinCodeAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// JoinTarget is the binding behind one join code.
type JoinTarget struct {
	KeyID     string `json:"keyId,omitempty"` // set when the code belongs to a server key
	Endpoint  string `json:"endpoint,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

func ensureJoinCodesLocked() {
	if acc == nil {
		return
	}
	if acc.JoinCodes == nil {
		acc.JoinCodes = map[string]JoinTarget{}
	}
}

// generateJoinCodeLocked mints a unique 7-character code (like zjjkkky).
func generateJoinCodeLocked() string {
	ensureJoinCodesLocked()
	for attempt := 0; attempt < 64; attempt++ {
		raw := make([]byte, joinCodeLen)
		if _, err := rand.Read(raw); err != nil {
			break
		}
		sb := strings.Builder{}
		for _, b := range raw {
			sb.WriteByte(joinCodeAlphabet[int(b)%len(joinCodeAlphabet)])
		}
		code := sb.String()
		if _, taken := acc.JoinCodes[code]; !taken {
			return code
		}
	}
	// Extremely unlikely fallback: timestamp-derived code, still unique-checked.
	code := strings.ToLower(strings.TrimLeft(itoaJoin(int64(len(acc.JoinCodes)+1)), "0"))
	for len(code) < joinCodeLen {
		code = "x" + code
	}
	for {
		if _, taken := acc.JoinCodes[code]; !taken {
			return code
		}
		code = code[1:] + "z"
	}
}

func itoaJoin(n int64) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// ensureJoinCodeForKeyLocked lazily attaches a join code to an existing key
// (legacy keys created before join codes existed).
func ensureJoinCodeForKeyLocked(rec *ServerKey) string {
	ensureJoinCodesLocked()
	if rec.JoinCode != "" {
		if _, ok := acc.JoinCodes[rec.JoinCode]; !ok {
			acc.JoinCodes[rec.JoinCode] = JoinTarget{KeyID: rec.ID, Endpoint: rec.LastEndpoint, CreatedAt: time.Now().Unix()}
		}
		return rec.JoinCode
	}
	// Re-attach to an existing target if one somehow references this key.
	for code, t := range acc.JoinCodes {
		if t.KeyID == rec.ID {
			rec.JoinCode = code
			return code
		}
	}
	code := generateJoinCodeLocked()
	ep := rec.LastEndpoint
	if ep == "" {
		ep = rec.BoundEndpoint
	}
	acc.JoinCodes[code] = JoinTarget{KeyID: rec.ID, Endpoint: ep, CreatedAt: time.Now().Unix()}
	rec.JoinCode = code
	return code
}

// bindJoinCodeEndpoint keeps a key's join code pointed at the server's live
// endpoint (called on every authenticated heartbeat).
func bindJoinCodeEndpoint(keyID, endpoint string) {
	if keyID == "" {
		return
	}
	accMu.Lock()
	defer accMu.Unlock()
	ensureAccountsInitializedLocked()
	ensureJoinCodesLocked()
	var rec *ServerKey
	for i := range acc.ServerKeys {
		if acc.ServerKeys[i].ID == keyID {
			rec = &acc.ServerKeys[i]
			break
		}
	}
	if rec == nil {
		return
	}
	code := ensureJoinCodeForKeyLocked(rec)
	t := acc.JoinCodes[code]
	t.Endpoint = endpoint
	acc.JoinCodes[code] = t
	saveAccountsLocked()
}

// ensureJoinCodeForEndpointLocked returns the join code to DISPLAY for a
// server endpoint, assigning an auto code only when explicitly asked
// (heartbeat time). Display paths use joinCodeForEndpointLookup so a GET
// never mutates state.
func ensureJoinCodeForEndpointLocked(ep string) string {
	ensureJoinCodesLocked()
	// A key-owned code for this endpoint wins (that's the "per key" code).
	bestCode := ""
	bestCreated := int64(-1)
	for code, t := range acc.JoinCodes {
		if t.Endpoint != ep {
			continue
		}
		if t.KeyID != "" {
			active := false
			for i := range acc.ServerKeys {
				rec := &acc.ServerKeys[i]
				if rec.ID == t.KeyID && !rec.Revoked && keyClock().Unix() < rec.ExpiresAt {
					active = true
					break
				}
			}
			if !active {
				continue
			}
			if t.CreatedAt > bestCreated {
				bestCode, bestCreated = code, t.CreatedAt
			}
		} else if bestCode == "" && bestCreated < 0 {
			bestCode, bestCreated = code, t.CreatedAt
		}
	}
	if bestCode != "" {
		return bestCode
	}
	code := generateJoinCodeLocked()
	acc.JoinCodes[code] = JoinTarget{Endpoint: ep, CreatedAt: time.Now().Unix()}
	return code
}

// joinCodeForEndpointLookup returns the display join code for an endpoint
// without creating one (safe for GET paths).
func joinCodeForEndpointLookup(ep string) string {
	if ep == "" {
		return ""
	}
	accMu.Lock()
	defer accMu.Unlock()
	if acc == nil {
		return ""
	}
	ensureJoinCodesLocked()
	bestCode := ""
	bestCreated := int64(-1)
	for code, t := range acc.JoinCodes {
		if t.Endpoint != ep {
			continue
		}
		if t.KeyID != "" {
			active := false
			for i := range acc.ServerKeys {
				rec := &acc.ServerKeys[i]
				if rec.ID == t.KeyID && !rec.Revoked && keyClock().Unix() < rec.ExpiresAt {
					active = true
					break
				}
			}
			if !active {
				continue
			}
			if t.CreatedAt > bestCreated {
				bestCode, bestCreated = code, t.CreatedAt
			}
		} else if bestCode == "" && bestCreated < 0 {
			bestCode, bestCreated = code, t.CreatedAt
		}
	}
	return bestCode
}

// joinCodeForEndpoint assigns-if-missing (heartbeat time).
func joinCodeForEndpoint(ep string) string {
	if ep == "" {
		return ""
	}
	accMu.Lock()
	defer accMu.Unlock()
	ensureAccountsInitializedLocked()
	code := ensureJoinCodeForEndpointLocked(ep)
	saveAccountsLocked()
	return code
}

// isJoinCode reports whether a /join/<value> path segment is an opaque code
// rather than a raw ip:port endpoint.
func isJoinCode(s string) bool {
	if len(s) < 4 || len(s) > 12 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// resolveJoinCode maps a join code to the server's live endpoint. Codes die
// with their key (revoked/expired keys resolve to nothing).
func resolveJoinCode(code string) (string, bool) {
	code = strings.TrimSpace(strings.ToLower(code))
	if !isJoinCode(code) {
		return "", false
	}
	accMu.Lock()
	if acc == nil {
		accMu.Unlock()
		return "", false
	}
	ensureJoinCodesLocked()
	t, ok := acc.JoinCodes[code]
	if !ok {
		accMu.Unlock()
		return "", false
	}
	keyID := t.KeyID
	ep := t.Endpoint
	if keyID != "" {
		rec := (*ServerKey)(nil)
		for i := range acc.ServerKeys {
			if acc.ServerKeys[i].ID == keyID {
				rec = &acc.ServerKeys[i]
				break
			}
		}
		if rec == nil || rec.Revoked || keyClock().Unix() >= rec.ExpiresAt {
			accMu.Unlock()
			return "", false
		}
		if ep == "" {
			ep = rec.LastEndpoint
		}
		if ep == "" {
			ep = rec.BoundEndpoint
		}
	}
	accMu.Unlock()
	if ep == "" && keyID != "" {
		ep = reg.endpointByKeyID(keyID)
	}
	if ep == "" {
		return "", false
	}
	return ep, true
}

// revokeJoinCodesForKeyLocked kills every code owned by a revoked key.
func revokeJoinCodesForKeyLocked(keyID string) {
	if acc == nil || keyID == "" {
		return
	}
	ensureJoinCodesLocked()
	for code, t := range acc.JoinCodes {
		if t.KeyID == keyID {
			delete(acc.JoinCodes, code)
		}
	}
}
