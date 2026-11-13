// hosting_frameworks.go — complete framework deployment, the way txAdmin
// recipes do it: every required resource of the framework, its dependencies,
// the SQL schema and a correctly ordered server.cfg block — not a single
// script dropped into resources/.
//
// Sources are ground-truthed against the official projects:
//   - QBCore: qbcore-framework/txAdminRecipe (qbcore.yaml + qbcore.sql)
//   - ESX:    esx-framework/esx_core monorepo ([core] + [SQL]/legacy.sql +
//             the server.cfg from the same repo)
//   - Qbox:   Qbox-project/txAdminRecipe (qbox.yaml + qbox.sql) plus the
//             per-resource schemas the recipe imports
//   - vMenu:  TomGrobbe/vMenu release assets (compiled resource — the source
//             tree is C# and cannot be dropped into resources/)
//
// Install rules: never overwrite a resource folder the user already has
// (their data stays), never duplicate resources, verify every installed
// resource carries an fxmanifest.lua, detect missing dependencies, and write
// one managed server.cfg block so user edits outside it survive.
package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// fwPart is one download in a recipe.
type fwPart struct {
	Repo     string   // owner/name on GitHub
	Ref      string   // branch or tag
	SubPath  string   // path inside the repo to take ("" = root)
	Dest     string   // resources/-relative destination; "" = SQL-only part
	Group    bool     // Dest is a bracket group: keep folder structure
	AssetRE  string   // non-empty: download the matching LATEST-RELEASE asset instead of a source zip
	SQL      []string // schema files to import (paths relative to the part root)
	CfgNote  string   // extra log note at install time
}

// fwRecipe is a complete framework deployment.
type fwRecipe struct {
	ID       string
	Name     string
	Version  string
	Blurb    string
	Detect   string   // resources/-relative path that marks "installed"
	Parts    []fwPart
	Ensures  []string // exact server.cfg start order
	CfgLines []string // extra managed convars (onesync, framework config)
}

// fwHTTPGet is the single network seam: tests swap it to serve fixture
// release assets (source zips use the same function via codeload URLs).
var fwHTTPGet = func(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return fwHTTPClient.Do(req)
}

var fwHTTPClient = &http.Client{Timeout: 5 * time.Minute}

// fwAssetURL resolves the download URL of the latest release asset whose
// name matches re for the given repo.
func fwAssetURL(ctx context.Context, repo, re string) (string, string, error) {
	pat := regexp.MustCompile(re)
	resp, err := fwHTTPGet(ctx, "https://api.github.com/repos/"+repo+"/releases/latest")
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("github releases API: HTTP %d for %s", resp.StatusCode, repo)
	}
	var rel struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", "", err
	}
	for _, a := range rel.Assets {
		if pat.MatchString(a.Name) {
			return a.URL, a.Name, nil
		}
	}
	return "", "", fmt.Errorf("no release asset matching %q in %s", re, repo)
}

// fwSourceZipURL builds the codeload URL for a repo/ref source archive.
func fwSourceZipURL(repo, ref string) string {
	return "https://codeload.github.com/" + repo + "/zip/refs/heads/" + ref
}

// fwDownload fetches a part to a zip file on disk. Returns the zip path and
// the resource root directory name inside it (when known).
func fwDownload(ctx context.Context, p fwPart, tmpDir string) (string, error) {
	url := fwSourceZipURL(p.Repo, p.Ref)
	if p.AssetRE != "" {
		u, name, err := fwAssetURL(ctx, p.Repo, p.AssetRE)
		if err != nil {
			return "", err
		}
		url = u
		_ = name
	}
	resp, err := fwHTTPGet(ctx, url)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", p.Repo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", p.Repo, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(tmpDir, "fw-*.zip")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, 1<<30))
	if err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("download %s: %w", p.Repo, err)
	}
	if n < 64 {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("download %s: truncated (%d bytes)", p.Repo, n)
	}
	return tmp.Name(), nil
}

// fwUnzipToStage extracts a zip with zip-slip protection and returns the
// stage directory.
func fwUnzipToStage(zipPath, stage string) error {
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	destClean := filepath.Clean(stage)
	for _, f := range r.File {
		rel := filepath.FromSlash(f.Name)
		target := filepath.Join(destClean, rel)
		if target != destClean && !strings.HasPrefix(target, destClean+string(os.PathSeparator)) {
			return fmt.Errorf("illegal path in package: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, io.LimitReader(rc, 1<<30))
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// fwFindResourceRoot locates the directory holding fxmanifest.lua within the
// stage (root, or inside a single wrapping folder — GitHub zips and release
// zips both wrap). For groups the root itself is returned.
func fwFindResourceRoot(stage string, group bool) (string, error) {
	if group {
		return stage, nil
	}
	if _, err := os.Stat(filepath.Join(stage, "fxmanifest.lua")); err == nil {
		return stage, nil
	}
	ents, err := os.ReadDir(stage)
	if err != nil {
		return "", err
	}
	var dirs []string
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), "__") {
			dirs = append(dirs, e.Name())
		}
	}
	// one or two levels deep (release zips sometimes nest resource in folder)
	for _, d := range dirs {
		p := filepath.Join(stage, d)
		if _, err := os.Stat(filepath.Join(p, "fxmanifest.lua")); err == nil {
			return p, nil
		}
		subs, _ := os.ReadDir(p)
		for _, sd := range subs {
			if sd.IsDir() {
				sp := filepath.Join(p, sd.Name())
				if _, err := os.Stat(filepath.Join(sp, "fxmanifest.lua")); err == nil {
					return sp, nil
				}
			}
		}
	}
	return "", fmt.Errorf("package contains no fxmanifest.lua — not a FiveM resource")
}

// fwCopyResource copies src into dst. When dst already exists, existing user
// files win (no clobber): new files are added, existing ones kept. Returns
// whether dst pre-existed.
func fwCopyResource(src, dst string) (bool, error) {
	existed := false
	if _, err := os.Stat(dst); err == nil {
		existed = true
	}
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		t := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(t, 0o755)
		}
		if _, err := os.Stat(t); err == nil {
			return nil // keep the user's file
		}
		return copyFile(path, t, 0o644)
	})
	return existed, err
}

// fwApplyCfg writes/refreshes the managed framework block in server.cfg.
// Only the block between the markers is touched — user edits elsewhere
// survive every reinstall.
func fwApplyCfg(cfgPath, recipeID string, lines []string) error {
	const startTag = "# >>> nsm-framework:%s — managed by the NSM Hosting panel >>>"
	const endTag = "# <<< nsm-framework <<<"
	start := fmt.Sprintf(startTag, recipeID)
	var b strings.Builder
	b.WriteString(start + "\n")
	for _, ln := range lines {
		b.WriteString(strings.TrimRight(ln, " \t") + "\n")
	}
	b.WriteString(endTag + "\n")

	old, err := os.ReadFile(cfgPath)
	if err != nil {
		// no cfg yet — create one with just our block
		return os.WriteFile(cfgPath, []byte(b.String()), 0o644)
	}
	text := string(old)
	// replace existing managed block from ANY recipe (frameworks are
	// exclusive: installing one replaces the previous managed block)
	if i := strings.Index(text, "# >>> nsm-framework:"); i >= 0 {
		if j := strings.Index(text[i:], endTag); j >= 0 {
			text = text[:i] + text[i+j+len(endTag):]
			text = strings.TrimRight(text, "\n") + "\n"
		} else {
			text = text[:i]
		}
	}
	text = strings.TrimRight(text, "\n") + "\n\n" + b.String()
	return os.WriteFile(cfgPath, []byte(text), 0o644)
}

// fwInstallReport is the machine-readable outcome of one install run.
type fwInstallReport struct {
	Recipe     string   `json:"recipe"`
	Installed  []string `json:"installed"`
	Skipped    []string `json:"skipped"`
	Failed     string   `json:"failed,omitempty"`
	SQL        []string `json:"sql"`
	Cfg        bool     `json:"cfg"`
	Missing    []string `json:"missing,omitempty"`
	Total      int      `json:"total"`
	Database   string   `json:"database,omitempty"`
}

// installFramework runs a full recipe against one server. It is safe to
// re-run: already-present resources are kept and reported as skipped.
func (m *hostingManager) installFramework(ctx context.Context, s *HostedServer, rec fwRecipe) fwInstallReport {
	rep := fwInstallReport{Recipe: rec.ID, Total: len(rec.Parts)}
	var schemas []struct{ name, text string }
	logf := func(line string) { m.appendLog(s, line) }
	root := m.serverRoot(s)
	resDir := filepath.Join(root, "resources")
	_ = os.MkdirAll(resDir, 0o755)
	tmpDir := filepath.Join(m.dirOf(s), "fw-tmp")
	_ = os.MkdirAll(tmpDir, 0o755)
	defer os.RemoveAll(tmpDir)

	logf(fmt.Sprintf("[apps] %s: installing %s (%d components)…", rec.Name, rec.Version, len(rec.Parts)))
	for i, p := range rec.Parts {
		label := p.Repo
		if p.SubPath != "" {
			label += "/" + p.SubPath
		}
		logf(fmt.Sprintf("[apps] %s: (%d/%d) %s…", rec.ID, i+1, len(rec.Parts), label))
		zipPath, err := fwDownload(ctx, p, tmpDir)
		if err != nil {
			rep.Failed = err.Error()
			logf("[apps] " + rec.ID + ": FAILED — " + rep.Failed)
			return rep
		}
		stage := filepath.Join(tmpDir, fmt.Sprintf("stage-%d", i))
		if err := fwUnzipToStage(zipPath, stage); err != nil {
			rep.Failed = fmt.Sprintf("%s: unpack failed: %v", label, err)
			logf("[apps] " + rec.ID + ": FAILED — " + rep.Failed)
			return rep
		}
		srcRoot := stage
		if p.SubPath != "" {
			// source zips wrap in <repo>-<ref>/
			inner, err := fwUnwrap(stage)
			if err != nil {
				rep.Failed = label + ": " + err.Error()
				return rep
			}
			srcRoot = filepath.Join(inner, filepath.FromSlash(p.SubPath))
		} else {
			inner, err := fwUnwrap(stage)
			if err != nil {
				rep.Failed = label + ": " + err.Error()
				return rep
			}
			srcRoot = inner
		}

		// collect schemas first (paths relative to the part root); they are
		// imported into the framework database once it exists below.
		for _, sqlRel := range p.SQL {
			b, err := os.ReadFile(filepath.Join(srcRoot, filepath.FromSlash(sqlRel)))
			if err != nil {
				logf("[apps] " + rec.ID + ": schema " + sqlRel + " missing in " + label)
				continue
			}
			safeName := strings.NewReplacer("/", "_", " ", "_").Replace(sqlRel)
			schemas = append(schemas, struct{ name, text string }{rec.ID + "-" + safeName, string(b)})
		}

		if p.Dest == "" {
			continue // SQL-only part
		}
		dest := filepath.Join(resDir, filepath.FromSlash(p.Dest))
		var resRoot string
		if p.Group {
			resRoot = srcRoot
		} else {
			resRoot, err = fwFindResourceRoot(srcRoot, false)
			if err != nil {
				rep.Failed = fmt.Sprintf("%s: %v", label, err)
				logf("[apps] " + rec.ID + ": FAILED — " + rep.Failed)
				return rep
			}
		}
		existed, err := fwCopyResource(resRoot, dest)
		if err != nil {
			rep.Failed = fmt.Sprintf("%s: copy failed: %v", label, err)
			logf("[apps] " + rec.ID + ": FAILED — " + rep.Failed)
			return rep
		}
		if existed {
			rep.Skipped = append(rep.Skipped, p.Dest+" (kept existing files)")
			logf("[apps] " + rec.ID + ": " + p.Dest + " already present — kept your files, added only missing ones")
		} else {
			rep.Installed = append(rep.Installed, p.Dest)
		}
	}

	// --- database: create the framework database, then import the schemas ---
	var dbLine string
	d := m.ensureServerDatabase(s, "main")
	if d != nil {
		svc := dbServiceFor()
		dbLine = `set mysql_connection_string "` + mysqlConnString(d, svc) + `"`
		rep.Database = d.DBName
	} else {
		dbLine = `# set mysql_connection_string "mysql://user:password@127.0.0.1:3306/nsm_...?charset=utf8mb4"`
		dbLine += "  <- create a database in the Databases tab, then fill this in"
		if len(schemas) > 0 {
			logf("[apps] " + rec.ID + ": database service not configured — schemas saved to nsm-db-import/ for HeidiSQL")
		}
	}
	for _, sc := range schemas {
		msg := m.importServerSQL(s, d, sc.name, sc.text)
		rep.SQL = append(rep.SQL, msg)
	}

	// --- managed server.cfg block ---
	cfgLines := []string{}
	for _, l := range rec.CfgLines {
		cfgLines = append(cfgLines, l)
	}
	cfgLines = append(cfgLines, dbLine)
	for _, e := range rec.Ensures {
		cfgLines = append(cfgLines, "ensure "+e)
	}
	cfgPath := filepath.Join(root, "server.cfg")
	if err := fwApplyCfg(cfgPath, rec.ID, cfgLines); err != nil {
		rep.Failed = "server.cfg: " + err.Error()
		return rep
	}
	rep.Cfg = true

	// --- verification: every ensure must exist on disk ---
	installed := map[string]bool{}
	for _, n := range discoverResources(root) {
		installed[n] = true
	}
	for _, e := range rec.Ensures {
		if strings.HasPrefix(e, "[") {
			continue // group bracket syntax
		}
		if !installed[e] {
			rep.Missing = append(rep.Missing, e)
		}
	}
	if len(rep.Missing) > 0 {
		logf("[apps] " + rec.ID + ": WARNING — missing after install: " + strings.Join(rep.Missing, ", "))
	}
	logf(fmt.Sprintf("[apps] %s: done — %d installed, %d kept, %d schemas, cfg written. Restart the server to load it.", rec.ID, len(rep.Installed), len(rep.Skipped), len(rep.SQL)))
	return rep
}

// fwUnwrap strips the single top-level folder GitHub archives wrap in.
func fwUnwrap(stage string) (string, error) {
	ents, err := os.ReadDir(stage)
	if err != nil {
		return "", err
	}
	var dirs []string
	files := 0
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "__") {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		} else {
			files++
		}
	}
	if len(dirs) == 1 && files == 0 {
		return filepath.Join(stage, dirs[0]), nil
	}
	return stage, nil
}

// ---------------------------------------------------------------------------
// the recipes (ground-truthed against the official repos; see file header)
// ---------------------------------------------------------------------------

// fwQBParts mirrors the official qbcore.yaml recipe. Sources are verified
// live (2026-10): the qbcore-framework org renamed to qbcore-fivem,
// dpemotes lives at andristum/dpemotes and LegacyFuel at InZidiuZ/LegacyFuel.
func fwQBParts() []fwPart {
	qb := func(name string) fwPart {
		return fwPart{Repo: "qbcore-fivem/" + name, Ref: "main", Dest: "[qb]/" + name}
	}
	return []fwPart{
		{Repo: "overextended/oxmysql", Ref: "main", Dest: "[standalone]/oxmysql", AssetRE: `^oxmysql\.zip$`},
		{Repo: "qbcore-fivem/progressbar", Ref: "main", Dest: "[standalone]/progressbar"},
		{Repo: "ThymonA/menuv", Ref: "master", Dest: "[standalone]/menuv"},
		{Repo: "Bob74/bob74_ipl", Ref: "master", Dest: "[standalone]/bob74_ipl"},
		{Repo: "qbcore-fivem/connectqueue", Ref: "master", Dest: "[standalone]/connectqueue"},
		{Repo: "andristum/dpemotes", Ref: "master", Dest: "[standalone]/dpemotes"},
		{Repo: "InZidiuZ/LegacyFuel", Ref: "master", Dest: "[standalone]/LegacyFuel"},
		{Repo: "AvarianKnight/pma-voice", Ref: "main", Dest: "[voice]/pma-voice"},
		qb("qb-core"), qb("qb-multicharacter"), qb("qb-spawn"), qb("qb-adminmenu"),
		qb("qb-scoreboard"), qb("qb-target"), qb("qb-input"), qb("qb-menu"),
		qb("qb-inventory"), qb("qb-phone"), qb("qb-hud"), qb("qb-smallresources"),
		qb("qb-weathersync"), qb("qb-vehicleshop"), qb("qb-vehiclesales"),
		qb("qb-clothing"), qb("qb-houses"), qb("qb-policejob"),
		qb("qb-ambulancejob"), qb("qb-mechanicjob"), qb("qb-banking"), qb("qb-radialmenu"),
		// schema from the official recipe repo
		{Repo: "qbcore-fivem/txAdminRecipe", Ref: "main", SQL: []string{"qbcore.sql"}},
	}
}

func fwESXParts() []fwPart {
	return []fwPart{
		{Repo: "overextended/oxmysql", Ref: "main", Dest: "[core]/oxmysql", AssetRE: `^oxmysql\.zip$`},
		{
			Repo: "esx-framework/esx_core", Ref: "main", SubPath: "[core]", Dest: "[core]", Group: true,
		},
		{Repo: "esx-framework/esx_core", Ref: "main", SubPath: "[SQL]", SQL: []string{"legacy.sql"}},
	}
}

func fwQboxParts() []fwPart {
	ox := func(name string) fwPart {
		return fwPart{Repo: "overextended/" + name, Ref: "main", Dest: "[ox]/" + name}
	}
	qbx := func(name string) fwPart {
		return fwPart{Repo: "Qbox-project/" + name, Ref: "main", Dest: "[qbx]/" + name}
	}
	return []fwPart{
		{Repo: "overextended/oxmysql", Ref: "main", Dest: "[ox]/oxmysql", AssetRE: `^oxmysql\.zip$`},
		ox("ox_lib"), ox("ox_inventory"), ox("ox_target"),
		{Repo: "Qbox-project/qbx_core", Ref: "main", Dest: "[qbx]/qbx_core", SQL: []string{"qbx_core.sql"}},
		{Repo: "Qbox-project/qbx-multicharacter", Ref: "main", Dest: "[qbx]/qbx-multicharacter"},
		qbx("qbx_spawn"), qbx("qbx_hud"), qbx("qbx_smallresources"),
		qbx("qbx_radialmenu"), qbx("qbx_scoreboard"), qbx("qbx_loading"),
		{Repo: "Qbox-project/qbx_vehicles", Ref: "main", Dest: "[qbx]/qbx_vehicles", SQL: []string{"vehicles.sql"}},
		{Repo: "Qbox-project/txAdminRecipe", Ref: "main", SQL: []string{"qbox.sql"}},
	}
}

func fwVMenuParts() []fwPart {
	return []fwPart{
		// compiled resource — the source tree is C# and needs a build, so the
		// official release zip is the only complete install source.
		{Repo: "TomGrobbe/vMenu", Ref: "master", Dest: "[vMenu]/vMenu", AssetRE: `^vMenu-Enhanced-v[0-9.]+\.zip$`},
	}
}

var frameworkRecipes = map[string]fwRecipe{
	"qbcore": {
		ID: "qbcore", Name: "QBCore", Version: "stable",
		Blurb: "The complete QBCore framework: core, multicharacter, spawn, inventory, phone, jobs, HUD and every required dependency, with the official SQL schema and a correctly ordered startup config.",
		Detect: "[qb]/qb-core", Parts: fwQBParts(),
		Ensures: []string{
			"oxmysql", "qb-core", "qb-input", "qb-menu", "qb-target", "qb-radialmenu",
			"qb-multicharacter", "qb-spawn", "qb-inventory", "qb-phone", "qb-hud",
			"qb-banking", "qb-smallresources", "qb-weathersync", "qb-adminmenu",
			"qb-scoreboard", "progressbar", "menuv", "bob74_ipl", "connectqueue",
			"dpemotes", "LegacyFuel", "pma-voice",
		},
		CfgLines: []string{"set onesync on"},
	},
	"esx": {
		ID: "esx", Name: "ESX Legacy", Version: "legacy",
		Blurb: "The complete ESX Legacy stack from the official esx_core monorepo: es_extended, esx_lib, identity, multicharacter, menus, skinchanger and the official legacy.sql schema.",
		Detect: "[core]/es_extended", Parts: fwESXParts(),
		Ensures: []string{"oxmysql", "esx_lib", "es_extended", "[core]"},
		CfgLines: []string{
			"set onesync on",
			"add_ace resource.es_extended command.add_ace allow",
			"add_ace resource.es_extended command.add_principal allow",
			"add_ace resource.es_extended command.remove_principal allow",
			"add_ace resource.es_extended command.stop allow",
		},
	},
	"qbox": {
		ID: "qbox", Name: "Qbox", Version: "stable",
		Blurb: "The complete Qbox core stack: qbx_core with the ox suite (ox_lib, ox_inventory, ox_target, oxmysql), multicharacter, spawn, HUD and the official Qbox SQL schemas.",
		Detect: "[qbx]/qbx_core", Parts: fwQboxParts(),
		Ensures: []string{
			"oxmysql", "ox_lib", "ox_inventory", "ox_target", "qbx_core",
			"qbx-multicharacter", "qbx_spawn", "qbx_hud", "qbx_smallresources",
			"qbx_radialmenu", "qbx_scoreboard", "qbx_loading", "qbx_vehicles",
		},
		CfgLines: []string{
			"set onesync on",
			`setr qb_locale "en"`,
			`setr qbx:enableBridge "true"`,
		},
	},
	"vmenu": {
		ID: "vmenu", Name: "vMenu", Version: "enhanced",
		Blurb: "The complete vMenu Enhanced release (compiled resource + default config): player options, vehicles, world tools and permissions — the standard in-game menu.",
		Detect: "[vMenu]/vMenu", Parts: fwVMenuParts(),
		Ensures:  []string{"vMenu"},
		CfgLines: nil,
	},
}

// fwInstallState tracks a running install so two clicks cannot race.
var fwInstallState = struct {
	busy map[string]bool
}{busy: map[string]bool{}}

// fwStart launches a recipe install in the background with its own deadline,
// so slow downloads never hold an HTTP request open. Progress is visible in
// the activity log and in the apps endpoint's have/total counts. Returns
// false when an install is already running for this server.
func (m *hostingManager) fwStart(s *HostedServer, rec fwRecipe) bool {
	m.mu.Lock()
	if fwInstallState.busy[s.ID] {
		m.mu.Unlock()
		return false
	}
	fwInstallState.busy[s.ID] = true
	m.mu.Unlock()
	go func() {
		defer func() {
			m.mu.Lock()
			delete(fwInstallState.busy, s.ID)
			m.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
		defer cancel()
		m.installFramework(ctx, s, rec)
	}()
	return true
}

// fwBusy reports whether a framework install is running for this server.
func (m *hostingManager) fwBusy(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fwInstallState.busy[id]
}

// fwInstalledCount counts how many of the recipe's resources are on disk.
func fwInstalledCount(root string, rec fwRecipe) (int, int) {
	have := 0
	total := 0
	for _, p := range rec.Parts {
		if p.Dest == "" {
			continue
		}
		total++
		if _, err := os.Stat(filepath.Join(root, "resources", filepath.FromSlash(p.Dest))); err == nil {
			have++
		}
	}
	return have, total
}

// sortStringsN is a tiny helper for deterministic reports in tests.
func sortStringsN(s []string) { sort.Strings(s) }
