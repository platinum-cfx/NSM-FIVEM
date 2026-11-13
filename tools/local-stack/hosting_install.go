// hosting_install.go — installs the official NSM server files onto the VPS.
//
// Flow: the first hosted-city provisioning downloads the official
// nsm-server release package from our trusted release location over HTTPS,
// verifies its SHA-256 (published sidecar or NSM_SERVER_SHA256), unpacks it
// into the local template cache (server-files/), and every city then gets its
// own isolated copy of those files. Nothing runs from the shared template:
// each server executes the copy inside its own directory.
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultServerZipURL = "https://github.com/platinum-cfx/NSM-FIVEM/releases/download/nsm-v0.8.0-candidate/NSM-Server.zip"
	serverFilesDir      = "server-files"
	maxServerZipBytes   = 2 << 30 // hard safety cap
)

var (
	sfMu   sync.Mutex
	sfDir  string
	sfOK   bool
	sfWarn string // non-empty when the server files look foreign/stale
)

// serverFilesWarning returns a human warning when the server package does not
// look like the official NSM build (which carries the connection fixes).
func serverFilesWarning() string {
	sfMu.Lock()
	defer sfMu.Unlock()
	return sfWarn
}

// looksLikeNSMArtifact reports whether dir contains the official NSM server
// package (readme/START-SERVER markers written by the release pipeline).
func looksLikeNSMArtifact(dir string) bool {
	for _, sub := range []string{".", "NSM-Server"} {
		d := filepath.Join(dir, sub)
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			n := strings.ToLower(e.Name())
			if e.IsDir() {
				continue
			}
			if n == "start-server.bat" {
				return true
			}
			if n == "readme-server.txt" {
				if b, err := os.ReadFile(filepath.Join(d, e.Name())); err == nil && strings.Contains(string(b), "Powered by AFX") {
					return true
				}
			}
		}
	}
	return false
}

// ensureServerFiles returns the local template directory that holds the
// official server files, downloading and verifying them once if needed.
// Failures are NOT cached — a later retry (Start button) tries again.
func ensureServerFiles(logf func(string)) (string, error) {
	sfMu.Lock()
	defer sfMu.Unlock()
	if sfOK {
		return sfDir, nil
	}
	dir, err := resolveOrDownloadServerFiles(logf)
	if err != nil {
		return "", err
	}
	sfDir, sfOK = dir, true
	return sfDir, nil
}

func resolveOrDownloadServerFiles(logf func(string)) (string, error) {
	// 1. An explicitly configured binary always wins.
	if p := os.Getenv("NSM_SERVER_EXE"); p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			logf("[system] using server executable from NSM_SERVER_EXE: " + p)
			return filepath.Dir(p), nil
		}
		logf("[warn] NSM_SERVER_EXE is set but not found: " + p)
	}
	// 2. An existing install / template cache (skipped with NSM_SERVER_REFRESH=1).
	if os.Getenv("NSM_SERVER_REFRESH") != "1" {
		for _, dir := range []string{serverFilesDir, "."} {
			if exe := findServerExe(dir); exe != "" {
				logf("[system] server files found: " + exe)
				if dir == serverFilesDir && !looksLikeNSMArtifact(dir) {
					sfWarn = "these server files do not look like the official NSM build - players may be rejected with \"No authentication ticket was specified\" and resources may be missing. Delete the server-files folder (or set NSM_SERVER_REFRESH=1) and press Start to download the real package."
					logf("[system] WARNING: " + sfWarn)
				} else {
					sfWarn = ""
				}
				return dir, nil
			}
		}
	} else {
		logf("[system] NSM_SERVER_REFRESH=1 - ignoring the server-files cache, downloading fresh")
	}
	// 3. Download the official release package.
	return downloadServerFiles(logf)
}

// findServerExe returns the path of the server executable inside dir.
func findServerExe(dir string) string {
	for _, name := range []string{"NSM-Server.exe", "nsm-server.exe", "NSM-Server", "nsm-server"} {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	// one level deep (release zips often contain a top folder)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			if exe := findServerExeShallow(filepath.Join(dir, e.Name())); exe != "" {
				return exe
			}
		}
	}
	return ""
}

func findServerExeShallow(dir string) string {
	for _, name := range []string{"NSM-Server.exe", "nsm-server.exe", "NSM-Server", "nsm-server"} {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// serverZipURL is the one source of truth for the official NSM server
// package — the hosting installer downloads exactly this, and the homepage
// self-hosted download button serves exactly this.
func serverZipURL() string {
	if raw := strings.TrimSpace(os.Getenv("NSM_SERVER_ZIP_URL")); raw != "" {
		return raw
	}
	return defaultServerZipURL
}

func downloadServerFiles(logf func(string)) (string, error) {
	raw := serverZipURL()
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid server package URL: %w", err)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")) {
		return "", fmt.Errorf("refusing untrusted server package location %q — HTTPS required", raw)
	}
	logf("[system] downloading official server package from " + u.String())

	tmp, err := os.CreateTemp("", "nsm-server-*.zip")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(u.String())
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: HTTP %d from %s", resp.StatusCode, u.Host)
	}
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxServerZipBytes))
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	_ = tmp.Close()
	logf(fmt.Sprintf("[system] downloaded %.1f MB", float64(n)/(1<<20)))

	// --- integrity: SHA-256 ---
	sum, err := fileSHA256(tmpName)
	if err != nil {
		return "", err
	}
	expected := strings.TrimSpace(os.Getenv("NSM_SERVER_SHA256"))
	if expected == "" {
		// published sidecar next to the package: <url>.sha256
		if s, err := fetchText(client, u.String()+".sha256"); err == nil {
			fields := strings.Fields(strings.TrimSpace(s))
			if len(fields) > 0 {
				expected = strings.ToLower(fields[0])
			}
		}
	}
	if expected != "" {
		if !strings.EqualFold(strings.TrimSpace(expected), sum) {
			return "", fmt.Errorf("server package checksum mismatch: got %s, expected %s — refusing to install", sum, expected)
		}
		logf("[system] checksum verified: sha256 " + sum[:16] + "…")
	} else {
		logf("[warn] no published checksum for this build — package recorded as sha256 " + sum[:16] + "… (unverified)")
	}

	// --- unpack into the template cache ---
	if err := os.MkdirAll(serverFilesDir, 0o755); err != nil {
		return "", err
	}
	count, err := unzipSafe(tmpName, serverFilesDir)
	if err != nil {
		return "", fmt.Errorf("unpack failed: %w", err)
	}
	logf(fmt.Sprintf("[system] installed %d files into %s/", count, serverFilesDir))
	if exe := findServerExe(serverFilesDir); exe == "" {
		return "", fmt.Errorf("package did not contain the server executable")
	} else {
		logf("[system] server executable ready: " + exe)
	}
	return serverFilesDir, nil
}

func fetchText(client *http.Client, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")) {
		return "", fmt.Errorf("untrusted sidecar location")
	}
	resp, err := client.Get(raw)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return string(b), err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxServerZipBytes)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// unzipSafe extracts a zip with zip-slip protection.
func unzipSafe(zipPath, dest string) (int, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	destClean := filepath.Clean(dest)
	count := 0
	for _, f := range r.File {
		rel := filepath.FromSlash(f.Name)
		target := filepath.Join(destClean, rel)
		if target != destClean && !strings.HasPrefix(target, destClean+string(os.PathSeparator)) {
			return count, fmt.Errorf("illegal path in package: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return count, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return count, err
		}
		rc, err := f.Open()
		if err != nil {
			return count, err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			rc.Close()
			return count, err
		}
		_, err = io.Copy(out, io.LimitReader(rc, maxServerZipBytes))
		out.Close()
		rc.Close()
		if err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// installInstance copies the template server files into the city's own
// isolated directory and verifies the install. Returns the number of files
// installed and the instance-relative executable path.
func installInstance(templateDir, instDir string) (int, string, error) {
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		return 0, "", err
	}
	count := 0
	root := filepath.Clean(templateDir)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(instDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := copyFile(path, dst, info.Mode()&0o755|0o600); err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		return count, "", err
	}
	// verify the executable made it into the instance
	exe := findServerExe(instDir)
	if exe == "" {
		return count, "", fmt.Errorf("install verification failed: server executable missing in %s", instDir)
	}
	relExe, _ := filepath.Rel(instDir, exe)
	return count, relExe, nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
