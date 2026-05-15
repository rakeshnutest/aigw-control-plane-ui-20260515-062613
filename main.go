package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"github.com/robfig/cron/v3"
)

type App struct {
	db      *sql.DB
	cron    *cron.Cron
	cronIDs map[int64]cron.EntryID
	mu      sync.Mutex
}

type EnvoyTarget struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	LastStatus string `json:"lastStatus"`
}

type GitRepo struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	Namespace string `json:"namespace"`
}

type Skill struct {
	ID        string   `json:"id"`
	RepoID    string   `json:"repoId"`
	Namespace string   `json:"namespace"`
	Path      string   `json:"path"`
	Name      string   `json:"name"`
	Backend   string   `json:"backend"`
	Tools     []string `json:"tools"`
}

type ConsolidatedTool struct {
	Namespace string `json:"namespace"`
	Backend   string `json:"backend"`
	Tool      string `json:"tool"`
}

type PushSchedule struct {
	ID         int64  `json:"id"`
	Spec       string `json:"spec"`
	Enabled    bool   `json:"enabled"`
	LastRun    string `json:"lastRun"`
	LastStatus string `json:"lastStatus"`
}

type StateResp struct {
	CurrentUser      string                           `json:"currentUser"`
	IsAdmin          bool                             `json:"isAdmin"`
	Repos            []GitRepo                        `json:"repos"`
	Skills           []Skill                          `json:"skills"`
	Targets          []EnvoyTarget                    `json:"targets"`
	Users            []string                         `json:"users"`
	CurrentPolicies  map[string]bool                  `json:"currentPolicies"`
	AllPolicies      map[string]map[string]bool       `json:"allPolicies"`
	ConsolidatedView map[string][]ConsolidatedTool    `json:"consolidatedView"`
	Schedules        []PushSchedule                   `json:"schedules"`
	LastScanStatus   string                           `json:"lastScanStatus"`
	LastApplyStatus  string                           `json:"lastApplyStatus"`
}

type applyPayload struct {
	ConfigYAML string `json:"configYaml"`
}

func main() {
	dbPath := os.Getenv("CONTROL_PLANE_DB_PATH")
	if dbPath == "" {
		dbPath = "/tmp/control-plane-ui.db"
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatal(err)
	}

	app := &App{db: db, cron: cron.New(cron.WithSeconds()), cronIDs: map[int64]cron.EntryID{}}
	if err := app.initDB(); err != nil {
		log.Fatal(err)
	}
	if err := app.seedDefaults(); err != nil {
		log.Fatal(err)
	}
	if err := app.restoreSchedules(); err != nil {
		log.Fatal(err)
	}
	app.cron.Start()

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.home)
	mux.HandleFunc("/admin", app.adminPage)
	mux.HandleFunc("/user", app.userPage)

	mux.HandleFunc("/api/login", app.login)
	mux.HandleFunc("/api/logout", app.logout)
	mux.HandleFunc("/api/state", app.state)

	mux.HandleFunc("/api/admin/repos", app.adminRepos)
	mux.HandleFunc("/api/admin/repos/", app.adminDeleteRepo)
	mux.HandleFunc("/api/admin/scan", app.adminScan)

	mux.HandleFunc("/api/admin/targets", app.adminTargets)
	mux.HandleFunc("/api/admin/targets/", app.adminDeleteTarget)

	mux.HandleFunc("/api/admin/schedules", app.adminSchedules)
	mux.HandleFunc("/api/admin/schedules/", app.adminDeleteSchedule)

	mux.HandleFunc("/api/policies/toggle", app.togglePolicy)
	mux.HandleFunc("/api/admin/apply", app.adminApply)
	mux.HandleFunc("/adapter/policy/apply", app.localAdapterApply)

	addr := ":18081"
	log.Printf("control-plane demo listening at http://127.0.0.1%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func (a *App) initDB() error {
	stmts := []string{
		`create table if not exists users (username text primary key, password text not null, is_admin integer not null default 0);`,
		`create table if not exists repos (id text primary key, url text not null, namespace text not null);`,
		`create table if not exists skills (id text primary key, repo_id text not null, namespace text not null, path text not null, name text not null, backend text not null, tools_json text not null);`,
		`create table if not exists user_policies (username text not null, skill_id text not null, tool text not null, enabled integer not null, primary key(username,skill_id,tool));`,
		`create table if not exists targets (id text primary key, name text not null, url text not null, last_status text not null default '');`,
		`create table if not exists schedules (id integer primary key autoincrement, spec text not null, enabled integer not null default 1, last_run text not null default '', last_status text not null default '');`,
		`create table if not exists meta (key text primary key, value text not null);`,
	}
	for _, s := range stmts {
		if _, err := a.db.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) seedDefaults() error {
	_, err := a.db.Exec(`insert or ignore into users(username,password,is_admin) values ('admin','admin',1),('user-1','user-1',0)`)
	if err != nil {
		return err
	}
	_, err = a.db.Exec(`insert or ignore into targets(id,name,url,last_status) values ('local','Local Envoy (Docker)','http://127.0.0.1:18081/adapter/policy/apply','')`)
	return err
}

func (a *App) restoreSchedules() error {
	rows, err := a.db.Query(`select id,spec,enabled from schedules`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var spec string
		var enabled int
		if err := rows.Scan(&id, &spec, &enabled); err != nil {
			return err
		}
		if enabled == 1 {
			if err := a.startSchedule(id, spec); err != nil {
				log.Printf("failed restore schedule %d: %v", id, err)
			}
		}
	}
	return nil
}

func (a *App) startSchedule(id int64, spec string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	entryID, err := a.cron.AddFunc(spec, func() {
		status := a.applyPoliciesInternal()
		now := time.Now().Format(time.RFC3339)
		_, _ = a.db.Exec(`update schedules set last_run=?, last_status=? where id=?`, now, status, id)
	})
	if err != nil {
		return err
	}
	a.cronIDs[id] = entryID
	return nil
}

func (a *App) stopSchedule(id int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if eid, ok := a.cronIDs[id]; ok {
		a.cron.Remove(eid)
		delete(a.cronIDs, id)
	}
}

func (a *App) home(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte(`<html><body style="font-family:Arial;padding:20px"><h2>Control Plane Demo</h2><p><a href="/admin">Admin Panel</a></p><p><a href="/user">User Panel</a></p></body></html>`))
}

func (a *App) adminPage(w http.ResponseWriter, _ *http.Request) { _ = template.Must(template.New("admin").Parse(adminHTML)).Execute(w, nil) }
func (a *App) userPage(w http.ResponseWriter, _ *http.Request)  { _ = template.Must(template.New("user").Parse(userHTML)).Execute(w, nil) }

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "method not allowed", 405); return }
	var req struct{ Username, Password string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil { http.Error(w, err.Error(), 400); return }
	var dbPass string
	var admin int
	if err := a.db.QueryRow(`select password,is_admin from users where username=?`, req.Username).Scan(&dbPass, &admin); err != nil || dbPass != req.Password {
		http.Error(w, "invalid credentials", 403); return
	}
	http.SetCookie(w, &http.Cookie{Name: "cp_user", Value: req.Username, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: "cp_role", Value: map[bool]string{true: "admin", false: "user"}[admin == 1], Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, map[string]any{"ok": true, "username": req.Username, "isAdmin": admin == 1})
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "method not allowed", 405); return }
	http.SetCookie(w, &http.Cookie{Name: "cp_user", Value: "", Path: "/", MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: "cp_role", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, map[string]any{"ok": true})
}

func currentUser(r *http.Request) (string, bool) {
	u, _ := r.Cookie("cp_user")
	role, _ := r.Cookie("cp_role")
	if u == nil || u.Value == "" {
		return "", false
	}
	return u.Value, role != nil && role.Value == "admin"
}

func requireAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	u, admin := currentUser(r)
	if u == "" || !admin {
		http.Error(w, "admin login required", 401)
		return "", false
	}
	return u, true
}

func (a *App) state(w http.ResponseWriter, r *http.Request) {
	u, admin := currentUser(r)
	if u == "" {
		writeJSON(w, StateResp{CurrentUser: "", IsAdmin: false})
		return
	}
	skills := a.listSkills()
	resp := StateResp{
		CurrentUser:      u,
		IsAdmin:          admin,
		Repos:            a.listRepos(),
		Skills:           skills,
		Targets:          a.listTargets(),
		Users:            a.listUsers(),
		CurrentPolicies:  a.policyForUser(u),
		AllPolicies:      a.allPolicies(),
		ConsolidatedView: a.consolidated(skills),
		Schedules:        a.listSchedules(),
		LastScanStatus:   a.meta("last_scan_status"),
		LastApplyStatus:  a.meta("last_apply_status"),
	}
	if !admin {
		resp.Repos = nil
		resp.Targets = nil
		resp.Schedules = nil
		resp.AllPolicies = nil
		resp.ConsolidatedView = nil
		resp.Users = []string{u}
	}
	writeJSON(w, resp)
}

func (a *App) adminRepos(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok { return }
	if r.Method != http.MethodPost { http.Error(w, "method not allowed", 405); return }
	var req struct{ URL, Namespace string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil { http.Error(w, err.Error(), 400); return }
	if !isValidNamespace(req.Namespace) { http.Error(w, "invalid namespace", 400); return }
	if _, _, err := parseGitHubRepo(req.URL); err != nil { http.Error(w, err.Error(), 400); return }
	id := slug(req.URL + ":" + req.Namespace)
	_, _ = a.db.Exec(`insert or ignore into repos(id,url,namespace) values(?,?,?)`, id, req.URL, req.Namespace)
	a.state(w, r)
}

func (a *App) adminDeleteRepo(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok { return }
	if r.Method != http.MethodDelete { http.Error(w, "method not allowed", 405); return }
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/repos/")
	_, _ = a.db.Exec(`delete from repos where id=?`, id)
	_, _ = a.db.Exec(`delete from skills where repo_id=?`, id)
	a.state(w, r)
}

func (a *App) adminScan(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok { return }
	if r.Method != http.MethodPost { http.Error(w, "method not allowed", 405); return }
	repos := a.listRepos(); all := []Skill{}
	for _, rp := range repos {
		owner, repo, err := parseGitHubRepo(rp.URL); if err != nil { continue }
		branch, err := fetchDefaultBranch(owner, repo); if err != nil { continue }
		files, err := fetchSkillFiles(owner, repo, branch); if err != nil { continue }
		for _, p := range files {
			sk, err := fetchSkillFromFile(owner, repo, branch, p); if err != nil { continue }
			sk.ID = slug(rp.ID + "::" + p); sk.RepoID = rp.ID; sk.Namespace = rp.Namespace
			if sk.Name == "" { sk.Name = pathBase(p) }
			if sk.Backend == "" { sk.Backend = "kiwi" }
			if len(sk.Tools) == 0 { sk.Tools = []string{"search-flight"} }
			all = append(all, sk)
		}
	}
	if len(all) == 0 {
		all = []Skill{{ID: "demo", RepoID: "demo", Namespace: "default", Path: "SKILL.md", Name: "github-pr-skill", Backend: "skill-github", Tools: []string{"pull_request_read", "pull_request_list"}}}
	}
	_, _ = a.db.Exec(`delete from skills`)
	for _, s := range all {
		b, _ := json.Marshal(s.Tools)
		_, _ = a.db.Exec(`insert into skills(id,repo_id,namespace,path,name,backend,tools_json) values(?,?,?,?,?,?,?)`, s.ID, s.RepoID, s.Namespace, s.Path, s.Name, s.Backend, string(b))
	}
	for _, u := range a.listUsers() {
		for _, s := range all {
			for _, t := range s.Tools {
				_, _ = a.db.Exec(`insert or ignore into user_policies(username,skill_id,tool,enabled) values(?,?,?,1)`, u, s.ID, t)
			}
		}
	}
	a.setMeta("last_scan_status", fmt.Sprintf("Discovered %d skill(s) from %d admin repo link(s)", len(all), len(repos)))
	a.state(w, r)
}

func (a *App) adminTargets(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok { return }
	if r.Method != http.MethodPost { http.Error(w, "method not allowed", 405); return }
	var req struct{ Name, URL string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil { http.Error(w, err.Error(), 400); return }
	u, err := url.Parse(req.URL)
	if err != nil || u.Scheme == "" || u.Host == "" { http.Error(w, "invalid target URL", 400); return }
	id := slug(req.Name + "-" + req.URL)
	_, _ = a.db.Exec(`insert or ignore into targets(id,name,url,last_status) values(?,?,?,'')`, id, req.Name, req.URL)
	a.state(w, r)
}

func (a *App) adminDeleteTarget(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok { return }
	if r.Method != http.MethodDelete { http.Error(w, "method not allowed", 405); return }
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/targets/")
	if id == "local" { a.state(w, r); return }
	_, _ = a.db.Exec(`delete from targets where id=?`, id)
	a.state(w, r)
}

func (a *App) adminSchedules(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok { return }
	if r.Method != http.MethodPost { http.Error(w, "method not allowed", 405); return }
	var req struct{ Spec string `json:"spec"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil { http.Error(w, err.Error(), 400); return }
	res, err := a.db.Exec(`insert into schedules(spec,enabled,last_run,last_status) values(?,1,'','')`, req.Spec)
	if err != nil { http.Error(w, err.Error(), 400); return }
	id, _ := res.LastInsertId()
	if err := a.startSchedule(id, req.Spec); err != nil {
		_, _ = a.db.Exec(`delete from schedules where id=?`, id)
		http.Error(w, "invalid cron spec: "+err.Error(), 400)
		return
	}
	a.state(w, r)
}

func (a *App) adminDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok { return }
	if r.Method != http.MethodDelete { http.Error(w, "method not allowed", 405); return }
	idStr := strings.TrimPrefix(r.URL.Path, "/api/admin/schedules/")
	var id int64
	_, _ = fmt.Sscan(idStr, &id)
	a.stopSchedule(id)
	_, _ = a.db.Exec(`delete from schedules where id=?`, id)
	a.state(w, r)
}

func (a *App) togglePolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "method not allowed", 405); return }
	u, admin := currentUser(r)
	if u == "" { http.Error(w, "login required", 401); return }
	var req struct{ User, SkillID, Tool string; Enabled bool }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil { http.Error(w, err.Error(), 400); return }
	if req.User == "" || !admin { req.User = u }
	_, _ = a.db.Exec(`insert into user_policies(username,skill_id,tool,enabled) values(?,?,?,?) on conflict(username,skill_id,tool) do update set enabled=excluded.enabled`, req.User, req.SkillID, req.Tool, boolToInt(req.Enabled))
	a.state(w, r)
}

func (a *App) adminApply(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok { return }
	if r.Method != http.MethodPost { http.Error(w, "method not allowed", 405); return }
	status := a.applyPoliciesInternal()
	a.setMeta("last_apply_status", status)
	a.state(w, r)
}

func (a *App) applyPoliciesInternal() string {
	skills := a.listSkills(); policies := a.allPolicies(); targets := a.listTargets()
	cfg := renderConfig(skills, policies)
	payload, _ := json.Marshal(applyPayload{ConfigYAML: cfg})
	parts := []string{}
	for _, t := range targets {
		st := pushToTarget(t.URL, payload)
		_, _ = a.db.Exec(`update targets set last_status=? where id=?`, st, t.ID)
		parts = append(parts, fmt.Sprintf("%s: %s", t.Name, st))
	}
	if len(parts) == 0 { return "no targets configured" }
	return strings.Join(parts, " | ")
}

func (a *App) localAdapterApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "method not allowed", 405); return }
	var p applyPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil { http.Error(w, err.Error(), 400); return }
	cfgPath := "/tmp/aigw-dyn/config.yaml"
	_ = os.MkdirAll("/tmp/aigw-dyn", 0o755)
	if err := os.WriteFile(cfgPath, []byte(p.ConfigYAML), 0o644); err != nil { http.Error(w, err.Error(), 500); return }
	if out, err := run("docker", "rm", "-f", "aigw-mcp-test"); err != nil && !strings.Contains(out, "No such container") { http.Error(w, fmt.Sprintf("failed remove: %v: %s", err, out), 500); return }
	out, err := run("docker", "run", "-d", "--name", "aigw-mcp-test", "-p", "1975:1975", "-v", cfgPath+":/etc/aigw/config.yaml:ro", "aigw-local-wiring:dev", "run", "/etc/aigw/config.yaml")
	if err != nil { http.Error(w, fmt.Sprintf("failed start: %v: %s", err, out), 500); return }
	writeJSON(w, map[string]string{"status": "ok", "container": strings.TrimSpace(out)})
}

func (a *App) listRepos() []GitRepo { rows, _ := a.db.Query(`select id,url,namespace from repos order by id`); defer rows.Close(); out := []GitRepo{}; for rows.Next() { var x GitRepo; _ = rows.Scan(&x.ID, &x.URL, &x.Namespace); out = append(out, x) }; return out }
func (a *App) listSkills() []Skill { rows, _ := a.db.Query(`select id,repo_id,namespace,path,name,backend,tools_json from skills order by id`); defer rows.Close(); out := []Skill{}; for rows.Next() { var s Skill; var tj string; _ = rows.Scan(&s.ID, &s.RepoID, &s.Namespace, &s.Path, &s.Name, &s.Backend, &tj); _ = json.Unmarshal([]byte(tj), &s.Tools); out = append(out, s) }; return out }
func (a *App) listTargets() []EnvoyTarget { rows, _ := a.db.Query(`select id,name,url,last_status from targets order by id`); defer rows.Close(); out := []EnvoyTarget{}; for rows.Next() { var t EnvoyTarget; _ = rows.Scan(&t.ID, &t.Name, &t.URL, &t.LastStatus); out = append(out, t) }; return out }
func (a *App) listUsers() []string { rows, _ := a.db.Query(`select username from users order by username`); defer rows.Close(); out := []string{}; for rows.Next() { var u string; _ = rows.Scan(&u); out = append(out, u) }; return out }
func (a *App) listSchedules() []PushSchedule { rows, _ := a.db.Query(`select id,spec,enabled,last_run,last_status from schedules order by id`); defer rows.Close(); out := []PushSchedule{}; for rows.Next() { var s PushSchedule; var enabled int; _ = rows.Scan(&s.ID, &s.Spec, &enabled, &s.LastRun, &s.LastStatus); s.Enabled = enabled == 1; out = append(out, s) }; return out }
func (a *App) policyForUser(u string) map[string]bool { if u == "" { return map[string]bool{} }; rows, _ := a.db.Query(`select skill_id,tool,enabled from user_policies where username=?`, u); defer rows.Close(); out := map[string]bool{}; for rows.Next() { var s, t string; var e int; _ = rows.Scan(&s, &t, &e); out[s+"::"+t] = e == 1 }; return out }
func (a *App) allPolicies() map[string]map[string]bool { out := map[string]map[string]bool{}; rows, _ := a.db.Query(`select username,skill_id,tool,enabled from user_policies`); defer rows.Close(); for rows.Next() { var u, s, t string; var e int; _ = rows.Scan(&u, &s, &t, &e); if out[u] == nil { out[u] = map[string]bool{} }; out[u][s+"::"+t] = e == 1 }; return out }
func (a *App) consolidated(skills []Skill) map[string][]ConsolidatedTool {
	skillByID := map[string]Skill{}; for _, s := range skills { skillByID[s.ID] = s }
	all := a.allPolicies(); out := map[string][]ConsolidatedTool{}
	for u, m := range all { arr := []ConsolidatedTool{}; for k, en := range m { if !en { continue }; sid, tool := splitKey(k); sk, ok := skillByID[sid]; if !ok { continue }; arr = append(arr, ConsolidatedTool{Namespace: sk.Namespace, Backend: sk.Backend, Tool: tool}) }; sort.Slice(arr, func(i,j int) bool { if arr[i].Namespace == arr[j].Namespace { if arr[i].Backend == arr[j].Backend { return arr[i].Tool < arr[j].Tool }; return arr[i].Backend < arr[j].Backend }; return arr[i].Namespace < arr[j].Namespace }); out[u] = arr }
	return out
}
func (a *App) meta(k string) string { var v string; _ = a.db.QueryRow(`select value from meta where key=?`, k).Scan(&v); return v }
func (a *App) setMeta(k, v string) { _, _ = a.db.Exec(`insert into meta(key,value) values(?,?) on conflict(key) do update set value=excluded.value`, k, v) }

func splitKey(k string) (string, string) { p := strings.SplitN(k, "::", 2); if len(p) != 2 { return "", "" }; return p[0], p[1] }
func boolToInt(v bool) int { if v { return 1 }; return 0 }
func pushToTarget(targetURL string, payload []byte) string { req, _ := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(payload)); req.Header.Set("content-type", "application/json"); resp, err := http.DefaultClient.Do(req); if err != nil { return "failed: " + err.Error() }; defer resp.Body.Close(); if resp.StatusCode >= 300 { var b bytes.Buffer; _, _ = b.ReadFrom(resp.Body); return fmt.Sprintf("failed: http %d %s", resp.StatusCode, strings.TrimSpace(b.String())) }; return "ok" }
func run(name string, args ...string) (string, error) { cmd := exec.Command(name, args...); var b bytes.Buffer; cmd.Stdout = &b; cmd.Stderr = &b; err := cmd.Run(); return b.String(), err }
func writeJSON(w http.ResponseWriter, v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }

func parseGitHubRepo(raw string) (owner, repo string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw)); if err != nil { return "", "", err }
	if u.Host != "github.com" { return "", "", fmt.Errorf("only github.com URLs are supported") }
	p := strings.Split(strings.Trim(u.Path, "/"), "/"); if len(p) < 2 { return "", "", fmt.Errorf("invalid GitHub repo URL") }
	return p[0], strings.TrimSuffix(p[1], ".git"), nil
}
func fetchDefaultBranch(owner, repo string) (string, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo)
	var b struct{ DefaultBranch string `json:"default_branch"` }
	if err := getJSON(u, &b); err != nil { return "", err }
	if b.DefaultBranch == "" { return "", fmt.Errorf("default branch is empty") }
	return b.DefaultBranch, nil
}
func fetchSkillFiles(owner, repo, branch string) ([]string, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/trees/%s?recursive=1", owner, repo, branch)
	var b struct{ Tree []struct{ Path string `json:"path"`; Type string `json:"type"` } `json:"tree"` }
	if err := getJSON(u, &b); err != nil { return nil, err }
	out := []string{}
	for _, n := range b.Tree { if n.Type == "blob" && strings.EqualFold(pathBase(n.Path), "SKILL.md") { out = append(out, n.Path) } }
	return out, nil
}
func fetchSkillFromFile(owner, repo, branch, path string) (Skill, error) {
	raw := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", owner, repo, branch, path)
	req, _ := http.NewRequest(http.MethodGet, raw, nil); req.Header.Set("User-Agent", "cp-demo")
	resp, err := http.DefaultClient.Do(req); if err != nil { return Skill{}, err }
	defer resp.Body.Close(); if resp.StatusCode >= 300 { return Skill{}, fmt.Errorf("status %d", resp.StatusCode) }
	var b bytes.Buffer; _, _ = b.ReadFrom(resp.Body); c := b.String()
	s := Skill{Path: path}
	for _, ln := range strings.Split(c, "\n") { t := strings.TrimSpace(ln); if strings.HasPrefix(t, "name:") { s.Name = strings.TrimSpace(strings.TrimPrefix(t, "name:")) }; if strings.HasPrefix(t, "backend:") { s.Backend = strings.TrimSpace(strings.TrimPrefix(t, "backend:")) } }
	inTools := false
	for _, ln := range strings.Split(c, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "tools:") { inTools = true; continue }
		if inTools {
			if strings.HasPrefix(t, "-") { v := strings.TrimSpace(strings.TrimPrefix(t, "-")); if v != "" { s.Tools = append(s.Tools, v) }; continue }
			if t == "" { continue }
			if strings.Contains(t, ":") { inTools = false }
		}
	}
	if len(s.Tools) == 0 {
		r := regexp.MustCompile(`(?m)^\s*-\s*([A-Za-z0-9_\-\.]+)\s*$`)
		m := r.FindAllStringSubmatch(c, -1)
		for _, mm := range m { v := strings.TrimSpace(mm[1]); if v != "" { s.Tools = append(s.Tools, v) } }
	}
	s.Tools = unique(s.Tools)
	return s, nil
}
func getJSON(u string, out any) error { req, _ := http.NewRequest(http.MethodGet, u, nil); req.Header.Set("User-Agent", "cp-demo"); resp, err := http.DefaultClient.Do(req); if err != nil { return err }; defer resp.Body.Close(); if resp.StatusCode >= 300 { var b bytes.Buffer; _, _ = b.ReadFrom(resp.Body); return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(b.String())) }; return json.NewDecoder(resp.Body).Decode(out) }
func unique(in []string) []string { seen := map[string]struct{}{}; out := []string{}; for _, v := range in { if _, ok := seen[v]; ok { continue }; seen[v] = struct{}{}; out = append(out, v) }; return out }
func pathBase(p string) string { i := strings.LastIndex(p, "/"); if i < 0 { return p }; return p[i+1:] }
func slug(s string) string { s = strings.ToLower(s); r := regexp.MustCompile(`[^a-z0-9]+`); s = r.ReplaceAllString(s, "-"); s = strings.Trim(s, "-"); if s == "" { return "target" }; return s }
func isValidNamespace(ns string) bool { return regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`).MatchString(ns) }

func renderConfig(skills []Skill, userPolicies map[string]map[string]bool) string {
	skillsByNS := map[string][]Skill{}
	for _, s := range skills { ns := s.Namespace; if ns == "" { ns = "default" }; skillsByNS[ns] = append(skillsByNS[ns], s) }
	nss := []string{}
	for ns := range skillsByNS { nss = append(nss, ns) }
	sort.Strings(nss)
	if len(nss) == 0 { nss = []string{"default"}; skillsByNS["default"] = []Skill{{ID: "demo", Namespace: "default", Backend: "kiwi", Tools: []string{"search-flight"}}} }

	var b strings.Builder
	for i, ns := range nss {
		if i > 0 { b.WriteString("---\n") }
		b.WriteString("apiVersion: aigateway.envoyproxy.io/v1beta1\nkind: MCPRoute\nmetadata:\n  name: mcp-main\n  namespace: "+ns+"\nspec:\n  parentRefs:\n    - name: aigw-run\n      kind: Gateway\n      group: gateway.networking.k8s.io\n  path: \"/mcp\"\n  backendRefs:\n")
		backendSet := map[string]struct{}{}
		for _, sk := range skillsByNS[ns] { backendSet[sk.Backend] = struct{}{} }
		if len(backendSet) == 0 { backendSet["kiwi"] = struct{}{} }
		backends := []string{}
		for be := range backendSet { backends = append(backends, be) }
		sort.Strings(backends)
		for _, be := range backends { b.WriteString("    - name: \""+be+"\"\n      kind: Backend\n      group: gateway.envoyproxy.io\n      path: \"/\"\n") }
		b.WriteString("  userToolPolicies:\n")
		users := []string{}
		for u := range userPolicies { users = append(users, u) }
		sort.Strings(users)
		sb := map[string]Skill{}; for _, sk := range skillsByNS[ns] { sb[sk.ID] = sk }
		for _, u := range users {
			b.WriteString("    - userId: \""+u+"\"\n      tools:\n")
			allowed := []ConsolidatedTool{}
			for k, en := range userPolicies[u] { if !en { continue }; sid, tool := splitKey(k); sk, ok := sb[sid]; if !ok { continue }; allowed = append(allowed, ConsolidatedTool{Namespace: sk.Namespace, Backend: sk.Backend, Tool: tool}) }
			sort.Slice(allowed, func(i, j int) bool { if allowed[i].Backend == allowed[j].Backend { return allowed[i].Tool < allowed[j].Tool }; return allowed[i].Backend < allowed[j].Backend })
			for _, a := range allowed { b.WriteString("        - backend: \""+a.Backend+"\"\n          tool: \""+a.Tool+"\"\n") }
		}
		b.WriteString("    - userId: \"*\"\n      tools:\n        - backend: \"kiwi\"\n          tool: \"search-flight\"\n")
	}
	b.WriteString(`---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: Backend
metadata:
  name: kiwi
  namespace: default
spec:
  endpoints:
    - fqdn:
        hostname: mcp.kiwi.com
        port: 443
---
apiVersion: gateway.networking.k8s.io/v1alpha3
kind: BackendTLSPolicy
metadata:
  name: kiwi-tls
  namespace: default
spec:
  targetRefs:
    - group: "gateway.envoyproxy.io"
      kind: Backend
      name: kiwi
  validation:
    wellKnownCACertificates: "System"
    hostname: mcp.kiwi.com
---
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: aigw-run
spec:
  controllerName: gateway.envoyproxy.io/gatewayclass-controller
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: aigw-run
  namespace: default
spec:
  gatewayClassName: aigw-run
  listeners:
    - name: http
      protocol: HTTP
      port: 1975
  infrastructure:
    parametersRef:
      group: gateway.envoyproxy.io
      kind: EnvoyProxy
      name: envoy-ai-gateway
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: EnvoyProxy
metadata:
  name: envoy-ai-gateway
  namespace: default
spec:
  logging:
    level:
      default: error
`)
	return b.String()
}

const adminHTML = `<!doctype html><html><head><meta charset="utf-8"><title>Admin Panel</title><style>body{font-family:Arial;margin:20px;max-width:1300px;background:#fafafa}.card{border:1px solid #ddd;padding:14px;border-radius:8px;margin-bottom:14px;background:#fff}button{padding:7px 10px;margin-right:6px}input,select{padding:6px}input[type=text],input[type=password]{width:380px}pre{background:#f7f7f7;padding:10px;white-space:pre-wrap}code{background:#f0f0f0;padding:2px 4px;border-radius:4px}table{width:100%;border-collapse:collapse;margin-top:8px}th,td{border:1px solid #ddd;padding:8px;text-align:left;vertical-align:top}th{background:#fafafa}.skills{display:grid;grid-template-columns:repeat(2,minmax(340px,1fr));gap:12px}.skill{border:1px solid #e5e5e5;border-radius:8px;padding:10px;background:#fcfcfc}</style></head><body><h2>Admin Panel</h2><p><a href="/user">Go to User Panel</a></p><div class="card"><h3>Login</h3><input id="u" placeholder="username"/><input id="p" type="password" placeholder="password"/><button onclick="login()">Login</button><button onclick="logout()">Logout</button><span id="auth"></span></div><div class="card"><h3>1) Admin Git links + namespace</h3><input id="repoUrl" type="text" placeholder="https://github.com/org/repo" value="https://github.com/rakeshnutest/skill-e2e-demo-20260515-060359"/><input id="repoNs" type="text" placeholder="namespace" value="default"/><button onclick="addRepo()">+ Add Git Link</button><button onclick="scanAll()">Scan All Links</button><table><thead><tr><th>Repo</th><th>Namespace</th><th>Action</th></tr></thead><tbody id="repos"></tbody></table><div id="scan"></div></div><div class="card"><h3>2) User policy editor</h3><label>User: <select id="userSel" onchange="render(window.__state)"></select></label><div class="skills" id="skills"></div></div><div class="card"><h3>3) Admin Envoy targets</h3><input id="tName" placeholder="target name"/><input id="tUrl" placeholder="http://host:port/adapter/policy/apply"/><button onclick="addTarget()">Add Target</button><table><thead><tr><th>Name</th><th>URL</th><th>Status</th><th>Action</th></tr></thead><tbody id="targets"></tbody></table></div><div class="card"><h3>4) Scheduled push (cron)</h3><input id="cronSpec" placeholder="*/30 * * * * *" value="*/30 * * * * *"/><button onclick="addSchedule()">Add Schedule</button><table><thead><tr><th>Spec</th><th>Last Run</th><th>Last Status</th><th>Action</th></tr></thead><tbody id="schedules"></tbody></table></div><div class="card"><h3>5) Consolidated user policies</h3><button onclick="apply()">Push Now</button><button onclick="load()">Refresh</button><table><thead><tr><th>User</th><th>namespace/backend/tool</th></tr></thead><tbody id="cons"></tbody></table></div><div class="card"><h3>State</h3><pre id="out"></pre></div><script>
function esc(s){return String(s??'').replace(/[&<>"']/g,m=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[m]))}
async function j(u,o={}){const r=await fetch(u,Object.assign({headers:{'content-type':'application/json'}},o));const t=await r.text();if(!r.ok) throw new Error(t); return t?JSON.parse(t):{}}
window.__state=null;
function en(s,user,skill,tool){const all=s.allPolicies||{}; const m=(all[user]||{}); return !!m[skill+'::'+tool]}
function render(s){window.__state=s; document.getElementById('out').textContent=JSON.stringify(s,null,2); document.getElementById('auth').textContent=(s.isAdmin?'Admin':'User')+': '+(s.currentUser||'not logged in'); if(!s.currentUser){document.getElementById('repos').innerHTML='';document.getElementById('skills').innerHTML='';document.getElementById('targets').innerHTML='';document.getElementById('schedules').innerHTML='';document.getElementById('cons').innerHTML='';document.getElementById('scan').textContent='Login to view details'; return;} document.getElementById('scan').textContent=s.lastScanStatus||'';
const users=s.users||[]; const sel=document.getElementById('userSel'); const cur=sel.value; sel.innerHTML=''; for(const u of users){const o=document.createElement('option'); o.value=u;o.textContent=u;sel.appendChild(o)}; if(users.includes(cur)) sel.value=cur;
const rb=document.getElementById('repos'); rb.innerHTML=''; for(const r of (s.repos||[])){const tr=document.createElement('tr'); tr.innerHTML='<td>'+esc(r.url)+'</td><td><code>'+esc(r.namespace)+'</code></td><td>'+(s.isAdmin?('<button onclick="delRepo(\''+esc(r.id)+'\')">Remove</button>'):'-')+'</td>'; rb.appendChild(tr)}
const sv=sel.value || ''; const root=document.getElementById('skills'); root.innerHTML=''; for(const sk of (s.skills||[])){const d=document.createElement('div'); d.className='skill'; let h='<h4>'+esc(sk.name)+'</h4><small>ns:<code>'+esc(sk.namespace)+'</code> backend:<code>'+esc(sk.backend)+'</code></small>'; for(const t of (sk.tools||[])){const c=en(s,sv,sk.id,t)?'checked':''; h+='<label style="display:block"><input type="checkbox" '+c+' onchange="toggleTool(\''+esc(sv)+'\',\''+esc(sk.id)+'\',\''+esc(t)+'\',this.checked)"/> <code>'+esc(t)+'</code></label>'} d.innerHTML=h; root.appendChild(d)}
const tb=document.getElementById('targets'); tb.innerHTML=''; for(const t of (s.targets||[])){const tr=document.createElement('tr'); tr.innerHTML='<td>'+esc(t.name)+'</td><td><code>'+esc(t.url)+'</code></td><td>'+esc(t.lastStatus||'')+'</td><td>'+(s.isAdmin&&t.id!=='local'?('<button onclick="delTarget(\''+esc(t.id)+'\')">Remove</button>'):'-')+'</td>'; tb.appendChild(tr)}
const sb=document.getElementById('schedules'); sb.innerHTML=''; for(const sc of (s.schedules||[])){const tr=document.createElement('tr'); tr.innerHTML='<td><code>'+esc(sc.spec)+'</code></td><td>'+esc(sc.lastRun||'')+'</td><td>'+esc(sc.lastStatus||'')+'</td><td><button onclick="delSchedule('+sc.id+')">Remove</button></td>'; sb.appendChild(tr)}
const cb=document.getElementById('cons'); cb.innerHTML=''; const cv=s.consolidatedView||{}; for(const u of Object.keys(cv).sort()){const items=(cv[u]||[]).map(x=>x.namespace+'/'+x.backend+'/'+x.tool).join('\n'); const tr=document.createElement('tr'); tr.innerHTML='<td><b>'+esc(u)+'</b></td><td><pre style="margin:0">'+esc(items||'(none)')+'</pre></td>'; cb.appendChild(tr)} }
async function load(){ try{render(await j('/api/state'))}catch(e){alert(e.message)} }
async function login(){ try{await j('/api/login',{method:'POST',body:JSON.stringify({username:document.getElementById('u').value,password:document.getElementById('p').value})}); await load()}catch(e){alert('login failed: '+e.message)} }
async function logout(){ try{await j('/api/logout',{method:'POST',body:'{}'}); await load()}catch(e){alert(e.message)} }
async function addRepo(){ try{render(await j('/api/admin/repos',{method:'POST',body:JSON.stringify({url:document.getElementById('repoUrl').value,namespace:document.getElementById('repoNs').value})}))}catch(e){alert(e.message)} }
async function delRepo(id){ try{render(await j('/api/admin/repos/'+encodeURIComponent(id),{method:'DELETE'}))}catch(e){alert(e.message)} }
async function scanAll(){ try{render(await j('/api/admin/scan',{method:'POST',body:'{}'}))}catch(e){alert(e.message)} }
async function addTarget(){ try{render(await j('/api/admin/targets',{method:'POST',body:JSON.stringify({name:document.getElementById('tName').value,url:document.getElementById('tUrl').value})}))}catch(e){alert(e.message)} }
async function delTarget(id){ try{render(await j('/api/admin/targets/'+encodeURIComponent(id),{method:'DELETE'}))}catch(e){alert(e.message)} }
async function addSchedule(){ try{render(await j('/api/admin/schedules',{method:'POST',body:JSON.stringify({spec:document.getElementById('cronSpec').value})}))}catch(e){alert(e.message)} }
async function delSchedule(id){ try{render(await j('/api/admin/schedules/'+id,{method:'DELETE'}))}catch(e){alert(e.message)} }
async function toggleTool(user,skillId,tool,enabled){ try{render(await j('/api/policies/toggle',{method:'POST',body:JSON.stringify({user,skillId,tool,enabled})}))}catch(e){alert(e.message)} }
async function apply(){ try{render(await j('/api/admin/apply',{method:'POST',body:'{}'}))}catch(e){alert(e.message)} }
load();
</script></body></html>`

const userHTML = `<!doctype html><html><head><meta charset="utf-8"><title>User Panel</title><style>body{font-family:Arial;margin:20px;max-width:1100px;background:#fafafa}.card{border:1px solid #ddd;padding:14px;border-radius:8px;margin-bottom:14px;background:#fff}button{padding:7px 10px;margin-right:6px}input{padding:6px}input[type=text],input[type=password]{width:280px}.skills{display:grid;grid-template-columns:repeat(2,minmax(320px,1fr));gap:12px}.skill{border:1px solid #e5e5e5;border-radius:8px;padding:10px;background:#fcfcfc}code{background:#f0f0f0;padding:2px 4px;border-radius:4px}</style></head><body><h2>User Panel</h2><div class="card"><h3>Login</h3><input id="u" placeholder="username"/><input id="p" type="password" placeholder="password"/><button onclick="login()">Login</button><button onclick="logout()">Logout</button><span id="auth"></span></div><div class="card"><h3>My Skills (select/deselect tools)</h3><div class="skills" id="skills"></div></div><script>
function esc(s){return String(s??'').replace(/[&<>"']/g,m=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[m]))}
async function j(u,o={}){const r=await fetch(u,Object.assign({headers:{'content-type':'application/json'}},o));const t=await r.text();if(!r.ok) throw new Error(t); return t?JSON.parse(t):{}}
window.__s=null;
function enabled(s,skill,tool){const m=s.currentPolicies||{}; return !!m[skill+'::'+tool]}
function render(s){window.__s=s; document.getElementById('auth').textContent=(s.currentUser?('User: '+s.currentUser):'not logged in'); const root=document.getElementById('skills'); root.innerHTML=''; if(!s.currentUser){return;} for(const sk of (s.skills||[])){const d=document.createElement('div'); d.className='skill'; let h='<h4>'+esc(sk.name)+'</h4><small>ns:<code>'+esc(sk.namespace)+'</code> backend:<code>'+esc(sk.backend)+'</code></small>'; for(const t of (sk.tools||[])){const c=enabled(s,sk.id,t)?'checked':''; h+='<label style="display:block"><input type="checkbox" '+c+' onchange="toggleTool(\''+esc(sk.id)+'\',\''+esc(t)+'\',this.checked)"/> <code>'+esc(t)+'</code></label>'} d.innerHTML=h; root.appendChild(d)} }
async function load(){ try{render(await j('/api/state'))}catch(e){alert(e.message)} }
async function login(){ try{await j('/api/login',{method:'POST',body:JSON.stringify({username:document.getElementById('u').value,password:document.getElementById('p').value})}); await load()}catch(e){alert('login failed: '+e.message)} }
async function logout(){ try{await j('/api/logout',{method:'POST',body:'{}'}); await load()}catch(e){alert(e.message)} }
async function toggleTool(skillId,tool,enabled){ try{render(await j('/api/policies/toggle',{method:'POST',body:JSON.stringify({skillId,tool,enabled})}))}catch(e){alert(e.message)} }
load();
</script></body></html>`
