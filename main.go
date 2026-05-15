package main

import (
	"bytes"
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
)

type envoyTarget struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	LastStatus string `json:"lastStatus"`
}

type gitRepo struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	Namespace string `json:"namespace"`
}

type discoveredSkill struct {
	ID        string   `json:"id"`
	RepoID    string   `json:"repoId"`
	Namespace string   `json:"namespace"`
	Path      string   `json:"path"`
	Name      string   `json:"name"`
	Backend   string   `json:"backend"`
	Tools     []string `json:"tools"`
}

type policyState struct {
	Repos            []gitRepo                     `json:"repos"`
	Skills           []discoveredSkill             `json:"skills"`
	UserPolicies     map[string]map[string]bool    `json:"userPolicies"` // user -> "skillID::tool"
	Targets          []envoyTarget                 `json:"targets"`
	LastScanStatus   string                        `json:"lastScanStatus"`
	LastApplyStatus  string                        `json:"lastApplyStatus"`
	ConsolidatedView map[string][]consolidatedTool `json:"consolidatedView"`
}

type consolidatedTool struct {
	Namespace string `json:"namespace"`
	Backend   string `json:"backend"`
	Tool      string `json:"tool"`
}

type applyPayload struct {
	ConfigYAML string `json:"configYaml"`
}

var (
	mu sync.RWMutex

	state = policyState{
		UserPolicies: map[string]map[string]bool{},
		Targets: []envoyTarget{{
			ID:   "local",
			Name: "Local Envoy (Docker)",
			URL:  "http://127.0.0.1:18081/adapter/policy/apply",
		}},
		ConsolidatedView: map[string][]consolidatedTool{},
	}
)


func isAdmin(r *http.Request) bool {
	c, err := r.Cookie("cp_admin")
	if err != nil {
		return false
	}
	return c.Value == "1"
}

func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if isAdmin(r) {
		return true
	}
	http.Error(w, "admin login required", http.StatusUnauthorized)
	return false
}

func adminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	adminToken := os.Getenv("CONTROL_PLANE_ADMIN_TOKEN")
	if adminToken == "" {
		adminToken = "admin"
	}
	if req.Token != adminToken {
		http.Error(w, "invalid admin token", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "cp_admin", Value: "1", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, map[string]any{"ok": true})
}

func adminLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "cp_admin", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, map[string]any{"ok": true})
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", ui)
	mux.HandleFunc("/api/state", getState)
	mux.HandleFunc("/api/admin/login", adminLogin)
	mux.HandleFunc("/api/admin/logout", adminLogout)

	mux.HandleFunc("/api/admin/repos", adminRepos)
	mux.HandleFunc("/api/admin/repos/", adminDeleteRepo)
	mux.HandleFunc("/api/admin/scan", adminScan)

	mux.HandleFunc("/api/admin/targets", adminTargets)
	mux.HandleFunc("/api/admin/targets/", adminDeleteTarget)

	mux.HandleFunc("/api/policies/toggle", toggleUserTool)
	mux.HandleFunc("/api/admin/apply", apply)

	mux.HandleFunc("/adapter/policy/apply", localAdapterApply)

	addr := ":18081"
	log.Printf("control plane UI listening at http://127.0.0.1%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func ui(w http.ResponseWriter, _ *http.Request) {
	tmpl := template.Must(template.New("ui").Parse(page))
	_ = tmpl.Execute(w, nil)
}

func getState(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	state.ConsolidatedView = buildConsolidatedView(state.Skills, state.UserPolicies)
	s := state
	mu.Unlock()
	writeJSON(w, map[string]any{"isAdmin": isAdmin(r), "state": s})
}

func adminRepos(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		URL       string `json:"url"`
		Namespace string `json:"namespace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Namespace) == "" {
		http.Error(w, "namespace is required", http.StatusBadRequest)
		return
	}
	if !isValidNamespace(req.Namespace) {
		http.Error(w, "invalid namespace", http.StatusBadRequest)
		return
	}
	_, _, err := parseGitHubRepo(req.URL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rp := gitRepo{ID: slug(req.URL + ":" + req.Namespace), URL: req.URL, Namespace: req.Namespace}
	mu.Lock()
	for _, existing := range state.Repos {
		if existing.URL == rp.URL && existing.Namespace == rp.Namespace {
			mu.Unlock()
			getState(w, r)
			return
		}
	}
	state.Repos = append(state.Repos, rp)
	mu.Unlock()
	getState(w, r)
}

func adminDeleteRepo(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/repos/")
	if id == "" {
		http.Error(w, "missing repo id", http.StatusBadRequest)
		return
	}
	mu.Lock()
	filtered := make([]gitRepo, 0, len(state.Repos))
	for _, rp := range state.Repos {
		if rp.ID != id {
			filtered = append(filtered, rp)
		}
	}
	state.Repos = filtered
	mu.Unlock()
	getState(w, r)
}

func adminScan(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mu.RLock()
	repos := append([]gitRepo(nil), state.Repos...)
	mu.RUnlock()

	allSkills := []discoveredSkill{}
	for _, rp := range repos {
		owner, repo, err := parseGitHubRepo(rp.URL)
		if err != nil {
			continue
		}
		branch, err := fetchDefaultBranch(owner, repo)
		if err != nil {
			continue
		}
		files, err := fetchSkillFiles(owner, repo, branch)
		if err != nil {
			continue
		}
		for _, p := range files {
			sk, err := fetchSkillFromFile(owner, repo, branch, p)
			if err != nil {
				continue
			}
			sk.RepoID = rp.ID
			sk.Namespace = rp.Namespace
			if sk.Name == "" {
				sk.Name = pathBase(p)
			}
			if sk.Backend == "" {
				sk.Backend = "kiwi"
			}
			if len(sk.Tools) == 0 {
				sk.Tools = []string{"search-flight"}
			}
			sk.ID = slug(rp.ID + "::" + p)
			allSkills = append(allSkills, sk)
		}
	}
	if len(allSkills) == 0 {
		allSkills = demoSkillsFromRepos(repos)
	}
	sort.Slice(allSkills, func(i, j int) bool { return allSkills[i].ID < allSkills[j].ID })

	mu.Lock()
	state.Skills = allSkills
	state.LastScanStatus = fmt.Sprintf("Discovered %d skill(s) from %d admin repo link(s)", len(allSkills), len(repos))
	if _, ok := state.UserPolicies["user-a"]; !ok {
		state.UserPolicies["user-a"] = map[string]bool{}
	}
	if _, ok := state.UserPolicies["user-b"]; !ok {
		state.UserPolicies["user-b"] = map[string]bool{}
	}
	for _, sk := range allSkills {
		for _, t := range sk.Tools {
			k := skillToolKey(sk.ID, t)
			if _, ok := state.UserPolicies["user-a"][k]; !ok {
				state.UserPolicies["user-a"][k] = true
			}
			if _, ok := state.UserPolicies["user-b"][k]; !ok {
				state.UserPolicies["user-b"][k] = true
			}
		}
	}
	mu.Unlock()
	getState(w, r)
}

func demoSkillsFromRepos(repos []gitRepo) []discoveredSkill {
	if len(repos) == 0 {
		repos = []gitRepo{{ID: "demo", Namespace: "default", URL: "https://github.com/rakeshnutest/skill-e2e-demo-20260515-060359"}}
	}
	ns := repos[0].Namespace
	rid := repos[0].ID
	return []discoveredSkill{
		{ID: "demo-kiwi", RepoID: rid, Namespace: ns, Path: "demo/kiwi/SKILL.md", Name: "Travel Search", Backend: "kiwi", Tools: []string{"search-flight", "feedback-to-devs"}},
		{ID: "demo-github", RepoID: rid, Namespace: ns, Path: "demo/github/SKILL.md", Name: "GitHub PR", Backend: "kiwi", Tools: []string{"pull_request_read", "pull_request_list"}},
		{ID: "demo-jira", RepoID: rid, Namespace: ns, Path: "demo/jira/SKILL.md", Name: "Jira", Backend: "kiwi", Tools: []string{"issue_get", "issue_search"}},
		{ID: "demo-slack", RepoID: rid, Namespace: ns, Path: "demo/slack/SKILL.md", Name: "Slack", Backend: "kiwi", Tools: []string{"channel_search", "thread_read"}},
		{ID: "demo-wiki", RepoID: rid, Namespace: ns, Path: "demo/wiki/SKILL.md", Name: "Wiki", Backend: "kiwi", Tools: []string{"wiki_search", "wiki_read"}},
	}
}

func adminTargets(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.URL) == "" {
		http.Error(w, "name and url are required", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(req.URL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		http.Error(w, "invalid target URL", http.StatusBadRequest)
		return
	}
	id := slug(req.Name + "-" + req.URL)
	mu.Lock()
	state.Targets = append(state.Targets, envoyTarget{ID: id, Name: req.Name, URL: req.URL})
	mu.Unlock()
	getState(w, r)
}

func adminDeleteTarget(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/targets/")
	if id == "" {
		http.Error(w, "missing target id", http.StatusBadRequest)
		return
	}
	mu.Lock()
	out := make([]envoyTarget, 0, len(state.Targets))
	for _, t := range state.Targets {
		if t.ID == id && t.ID == "local" {
			continue
		}
		if t.ID != id {
			out = append(out, t)
		}
	}
	state.Targets = out
	mu.Unlock()
	getState(w, r)
}

func toggleUserTool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		User    string `json:"user"`
		SkillID string `json:"skillId"`
		Tool    string `json:"tool"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.User) == "" {
		http.Error(w, "user is required", http.StatusBadRequest)
		return
	}
	mu.Lock()
	if state.UserPolicies == nil {
		state.UserPolicies = map[string]map[string]bool{}
	}
	if state.UserPolicies[req.User] == nil {
		state.UserPolicies[req.User] = map[string]bool{}
	}
	state.UserPolicies[req.User][skillToolKey(req.SkillID, req.Tool)] = req.Enabled
	mu.Unlock()
	getState(w, r)
}

func apply(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mu.RLock()
	skills := append([]discoveredSkill(nil), state.Skills...)
	ups := clonePolicies(state.UserPolicies)
	targets := append([]envoyTarget(nil), state.Targets...)
	mu.RUnlock()

	cfg := renderConfig(skills, ups)
	payloadBytes, _ := json.Marshal(applyPayload{ConfigYAML: cfg})

	statuses := make([]string, 0, len(targets))
	updated := make([]envoyTarget, 0, len(targets))
	for _, t := range targets {
		st := pushToTarget(t.URL, payloadBytes)
		t.LastStatus = st
		updated = append(updated, t)
		statuses = append(statuses, fmt.Sprintf("%s: %s", t.Name, st))
	}

	mu.Lock()
	state.Targets = updated
	state.LastApplyStatus = strings.Join(statuses, " | ")
	state.ConsolidatedView = buildConsolidatedView(state.Skills, state.UserPolicies)
	mu.Unlock()
	getState(w, r)
}

func clonePolicies(in map[string]map[string]bool) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for u, m := range in {
		out[u] = map[string]bool{}
		for k, v := range m {
			out[u][k] = v
		}
	}
	return out
}

func pushToTarget(targetURL string, payload []byte) string {
	req, _ := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(payload))
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "failed: " + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		return fmt.Sprintf("failed: http %d %s", resp.StatusCode, strings.TrimSpace(b.String()))
	}
	return "ok"
}

func localAdapterApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var p applyPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cfgPath := "/tmp/aigw-dyn/config.yaml"
	if err := os.MkdirAll("/tmp/aigw-dyn", 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(cfgPath, []byte(p.ConfigYAML), 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if out, err := run("docker", "rm", "-f", "aigw-mcp-test"); err != nil && !strings.Contains(out, "No such container") {
		http.Error(w, fmt.Sprintf("failed to remove old container: %v: %s", err, out), http.StatusInternalServerError)
		return
	}
	out, err := run("docker", "run", "-d",
		"--name", "aigw-mcp-test",
		"-p", "1975:1975",
		"-v", cfgPath+":/etc/aigw/config.yaml:ro",
		"aigw-local-wiring:dev",
		"run", "/etc/aigw/config.yaml",
	)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to start container: %v: %s", err, out), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "container": strings.TrimSpace(out)})
}

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &b
	err := cmd.Run()
	return b.String(), err
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func parseGitHubRepo(raw string) (owner, repo string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", err
	}
	if u.Host != "github.com" {
		return "", "", fmt.Errorf("only github.com URLs are supported")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid GitHub repo URL")
	}
	owner, repo = parts[0], strings.TrimSuffix(parts[1], ".git")
	return owner, repo, nil
}

func fetchDefaultBranch(owner, repo string) (string, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo)
	var body struct{ DefaultBranch string `json:"default_branch"` }
	if err := getJSON(u, &body); err != nil {
		return "", err
	}
	if body.DefaultBranch == "" {
		return "", fmt.Errorf("default branch is empty")
	}
	return body.DefaultBranch, nil
}

func fetchSkillFiles(owner, repo, branch string) ([]string, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/trees/%s?recursive=1", owner, repo, branch)
	var body struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err := getJSON(u, &body); err != nil {
		return nil, err
	}
	out := []string{}
	for _, n := range body.Tree {
		if n.Type == "blob" && strings.EqualFold(pathBase(n.Path), "SKILL.md") {
			out = append(out, n.Path)
		}
	}
	return out, nil
}

func fetchSkillFromFile(owner, repo, branch, path string) (discoveredSkill, error) {
	rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", owner, repo, branch, path)
	req, _ := http.NewRequest(http.MethodGet, rawURL, nil)
	req.Header.Set("User-Agent", "aigw-control-plane-ui")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return discoveredSkill{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return discoveredSkill{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	content := b.String()

	skill := discoveredSkill{Path: path}
	for _, line := range strings.Split(content, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "name:") {
			skill.Name = strings.TrimSpace(strings.TrimPrefix(trim, "name:"))
		}
		if strings.HasPrefix(trim, "backend:") {
			skill.Backend = strings.TrimSpace(strings.TrimPrefix(trim, "backend:"))
		}
	}

	// tools block parser
	lines := strings.Split(content, "\n")
	inTools := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "tools:") {
			inTools = true
			continue
		}
		if inTools {
			if strings.HasPrefix(t, "-") {
				name := strings.TrimSpace(strings.TrimPrefix(t, "-"))
				if name != "" {
					skill.Tools = append(skill.Tools, name)
				}
				continue
			}
			if t == "" {
				continue
			}
			if strings.Contains(t, ":") {
				inTools = false
			}
		}
	}

	if len(skill.Tools) == 0 {
		r := regexp.MustCompile(`(?m)^\s*-\s*([A-Za-z0-9_\-\.]+)\s*$`)
		m := r.FindAllStringSubmatch(content, -1)
		for _, mm := range m {
			t := strings.TrimSpace(mm[1])
			if t != "" {
				skill.Tools = append(skill.Tools, t)
			}
		}
	}
	skill.Tools = unique(skill.Tools)
	return skill, nil
}

func unique(in []string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func getJSON(u string, out any) error {
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("User-Agent", "aigw-control-plane-ui")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(b.String()))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func pathBase(p string) string {
	idx := strings.LastIndex(p, "/")
	if idx < 0 {
		return p
	}
	return p[idx+1:]
}

func slug(s string) string {
	s = strings.ToLower(s)
	r := regexp.MustCompile(`[^a-z0-9]+`)
	s = r.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "target"
	}
	return s
}

func isValidNamespace(ns string) bool {
	re := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	return re.MatchString(ns)
}

func skillToolKey(skillID, tool string) string {
	return skillID + "::" + tool
}

func splitSkillToolKey(k string) (string, string) {
	parts := strings.SplitN(k, "::", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func buildConsolidatedView(skills []discoveredSkill, userPolicies map[string]map[string]bool) map[string][]consolidatedTool {
	skillByID := map[string]discoveredSkill{}
	for _, s := range skills {
		skillByID[s.ID] = s
	}
	out := map[string][]consolidatedTool{}
	for user, toolsMap := range userPolicies {
		items := []consolidatedTool{}
		for key, enabled := range toolsMap {
			if !enabled {
				continue
			}
			skillID, tool := splitSkillToolKey(key)
			sk, ok := skillByID[skillID]
			if !ok {
				continue
			}
			items = append(items, consolidatedTool{Namespace: sk.Namespace, Backend: sk.Backend, Tool: tool})
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].Namespace == items[j].Namespace {
				if items[i].Backend == items[j].Backend {
					return items[i].Tool < items[j].Tool
				}
				return items[i].Backend < items[j].Backend
			}
			return items[i].Namespace < items[j].Namespace
		})
		out[user] = items
	}
	return out
}

func renderConfig(skills []discoveredSkill, userPolicies map[string]map[string]bool) string {
	// Group skills by namespace and build user policies per namespace.
	skillsByNamespace := map[string][]discoveredSkill{}
	for _, sk := range skills {
		ns := sk.Namespace
		if ns == "" {
			ns = "default"
		}
		skillsByNamespace[ns] = append(skillsByNamespace[ns], sk)
	}
	namespaces := make([]string, 0, len(skillsByNamespace))
	for ns := range skillsByNamespace {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)

	var b strings.Builder
	for i, ns := range namespaces {
		if i > 0 {
			b.WriteString("---\n")
		}
		b.WriteString("apiVersion: aigateway.envoyproxy.io/v1beta1\n")
		b.WriteString("kind: MCPRoute\n")
		b.WriteString("metadata:\n")
		b.WriteString("  name: mcp-main\n")
		b.WriteString("  namespace: ")
		b.WriteString(ns)
		b.WriteString("\n")
		b.WriteString("spec:\n")
		b.WriteString("  parentRefs:\n")
		b.WriteString("    - name: aigw-run\n")
		b.WriteString("      kind: Gateway\n")
		b.WriteString("      group: gateway.networking.k8s.io\n")
		b.WriteString("  path: \"/mcp\"\n")
		b.WriteString("  backendRefs:\n")

		backendSet := map[string]struct{}{}
		for _, sk := range skillsByNamespace[ns] {
			backendSet[sk.Backend] = struct{}{}
		}
		if len(backendSet) == 0 {
			backendSet["kiwi"] = struct{}{}
		}
		backendNames := make([]string, 0, len(backendSet))
		for be := range backendSet {
			backendNames = append(backendNames, be)
		}
		sort.Strings(backendNames)
		for _, be := range backendNames {
			b.WriteString("    - name: \"")
			b.WriteString(be)
			b.WriteString("\"\n")
			b.WriteString("      kind: Backend\n")
			b.WriteString("      group: gateway.envoyproxy.io\n")
			b.WriteString("      path: \"/\"\n")
		}

		b.WriteString("  userToolPolicies:\n")
		users := make([]string, 0, len(userPolicies))
		for u := range userPolicies {
			users = append(users, u)
		}
		sort.Strings(users)
		skillByID := map[string]discoveredSkill{}
		for _, sk := range skillsByNamespace[ns] {
			skillByID[sk.ID] = sk
		}

		for _, user := range users {
			b.WriteString("    - userId: \"")
			b.WriteString(user)
			b.WriteString("\"\n")
			b.WriteString("      tools:\n")
			allowed := []consolidatedTool{}
			for k, enabled := range userPolicies[user] {
				if !enabled {
					continue
				}
				skillID, tool := splitSkillToolKey(k)
				sk, ok := skillByID[skillID]
				if !ok {
					continue
				}
				allowed = append(allowed, consolidatedTool{Namespace: sk.Namespace, Backend: sk.Backend, Tool: tool})
			}
			sort.Slice(allowed, func(i, j int) bool {
				if allowed[i].Backend == allowed[j].Backend {
					return allowed[i].Tool < allowed[j].Tool
				}
				return allowed[i].Backend < allowed[j].Backend
			})
			for _, a := range allowed {
				b.WriteString("        - backend: \"")
				b.WriteString(a.Backend)
				b.WriteString("\"\n")
				b.WriteString("          tool: \"")
				b.WriteString(a.Tool)
				b.WriteString("\"\n")
			}
		}
		b.WriteString("    - userId: \"*\"\n")
		b.WriteString("      tools:\n")
		b.WriteString("        - backend: \"kiwi\"\n")
		b.WriteString("          tool: \"search-flight\"\n")
	}

	// Shared backend/gateway resources in default namespace for demo runtime.
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

const page = `<!doctype html>
<html><head><meta charset="utf-8"><title>AIGW Control Plane UI</title>
<style>
body{font-family:Arial;margin:20px;max-width:1300px;background:#fafafa}
.card{border:1px solid #ddd;padding:14px;border-radius:8px;margin-bottom:14px;background:#fff}
button{padding:7px 10px;margin-right:6px}
input,select{padding:6px}
input[type=text]{width:420px}
pre{background:#f7f7f7;padding:10px;white-space:pre-wrap}
code{background:#f0f0f0;padding:2px 4px;border-radius:4px}
table{width:100%;border-collapse:collapse;margin-top:8px}
th,td{border:1px solid #ddd;padding:8px;text-align:left;vertical-align:top}
th{background:#fafafa}
.badge{display:inline-block;padding:2px 8px;border-radius:999px;background:#eef}
.skills{display:grid;grid-template-columns:repeat(2,minmax(340px,1fr));gap:12px}
.skill{border:1px solid #e5e5e5;border-radius:8px;padding:10px;background:#fcfcfc}
.skill h4{margin:0 0 6px 0}
small{color:#555}
.row{display:flex;gap:8px;align-items:center;flex-wrap:wrap}
</style></head>
<body>
<h2>AIGW Control Plane UI - Admin</h2>
<div class="card">
  <h3>Admin Login</h3>
  <div class="row">
    <input id="adminToken" type="password" placeholder="admin token"/>
    <button onclick="adminLogin()">Login as Admin</button>
    <button onclick="adminLogout()">Logout</button>
    <span id="adminStatus"></span>
  </div>
</div>

<div class="card">
  <h3>1) Admin Git links (only admin adds repo links)</h3>
  <div class="row">
    <input id="repoUrl" type="text" placeholder="https://github.com/org/repo" value="https://github.com/rakeshnutest/skill-e2e-demo-20260515-060359"/>
    <input id="repoNs" type="text" placeholder="namespace (e.g. default)" value="default"/>
    <button onclick="addRepo()">Add Git Link</button>
    <button onclick="scanAll()">Scan All Links</button>
  </div>
  <table>
    <thead><tr><th>Repo URL</th><th>Namespace</th><th>Action</th></tr></thead>
    <tbody id="reposBody"></tbody>
  </table>
  <div id="scanStatus" style="margin-top:8px"></div>
</div>

<div class="card">
  <h3>2) User policy editor (per-tool enable/disable)</h3>
  <div class="row">
    <label>User:</label>
    <select id="userSelect" onchange="renderFromState(window.__state)"></select>
  </div>
  <div class="skills" id="skills"></div>
</div>

<div class="card">
  <h3>3) Envoy targets</h3>
  <div class="row">
    <input id="targetName" type="text" placeholder="Target name"/>
    <input id="targetUrl" type="text" placeholder="http://host:port/adapter/policy/apply"/>
    <button onclick="addTarget()">Add Target</button>
  </div>
  <table>
    <thead><tr><th>Name</th><th>Policy Push URL</th><th>Last status</th><th>Action</th></tr></thead>
    <tbody id="targetsBody"></tbody>
  </table>
</div>

<div class="card">
  <h3>4) Admin panel: consolidated user policies</h3>
  <button onclick="applyPolicy()">Push Consolidated Policies To All Targets</button>
  <button onclick="loadState()">Refresh</button>
  <table>
    <thead><tr><th>User</th><th>Allowed policies (namespace/backend/tool)</th></tr></thead>
    <tbody id="consolidatedBody"></tbody>
  </table>
</div>

<div class="card">
  <h3>State</h3>
  <pre id="out"></pre>
</div>

<script>
function esc(s){return String(s??'').replace(/[&<>"']/g,m=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[m]))}
async function j(url,opts={}){const r=await fetch(url,Object.assign({headers:{'content-type':'application/json'}},opts));const t=await r.text();if(!r.ok) throw new Error(t);return t?JSON.parse(t):{}}
window.__state = null;

function userToolEnabled(state,user,skillId,tool){
  const m=(state.userPolicies||{})[user]||{};
  return !!m[skillId+'::'+tool];
}

async function toggleUserTool(skillId,tool,enabled){
  try{
    const user=document.getElementById('userSelect').value;
    const s=await j('/api/policies/toggle',{method:'POST',body:JSON.stringify({user,skillId,tool,enabled})});
    renderFromState(s)
  }catch(e){ alert('Toggle failed: '+e.message) }
}

function renderFromState(resp){
  const state = resp.state || resp;
  window.__state = resp;
  document.getElementById("adminStatus").textContent = (resp.isAdmin?"Admin: logged in":"Admin: not logged in");
  document.getElementById('out').textContent = JSON.stringify(state,null,2)
  document.getElementById('scanStatus').innerHTML = '<span class="badge">'+esc(state.lastScanStatus||'No scan yet')+'</span>'

  const users = Object.keys(state.userPolicies||{});
  const sel = document.getElementById('userSelect');
  const current = sel.value;
  sel.innerHTML = '';
  for (const u of users.length?users:['user-a']){
    const opt=document.createElement('option'); opt.value=u; opt.textContent=u; sel.appendChild(opt);
  }
  if (users.includes(current)) sel.value = current;

  const reposBody=document.getElementById('reposBody'); reposBody.innerHTML='';
  for(const r of (state.repos||[])){
    const tr=document.createElement('tr');
    tr.innerHTML='<td>'+esc(r.url)+'</td><td><code>'+esc(r.namespace)+'</code></td><td><button onclick="delRepo(\''+esc(r.id)+'\')">Remove</button></td>';
    reposBody.appendChild(tr);
  }

  const selectedUser = document.getElementById('userSelect').value || 'user-a';
  const root=document.getElementById('skills'); root.innerHTML='';
  for(const sk of (state.skills||[])){
    const div=document.createElement('div'); div.className='skill';
    let html='<h4>'+esc(sk.name)+'</h4><small>ns: <code>'+esc(sk.namespace)+'</code> | backend: <code>'+esc(sk.backend)+'</code> | path: <code>'+esc(sk.path)+'</code></small><div style="margin-top:8px">';
    for(const t of (sk.tools||[])){
      const checked=userToolEnabled(state, selectedUser, sk.id, t)?'checked':'';
      html += '<label style="display:block;margin-bottom:4px"><input type="checkbox" '+checked+' onchange="toggleUserTool(\''+esc(sk.id)+'\',\''+esc(t)+'\',this.checked)"/> <code>'+esc(t)+'</code></label>';
    }
    html += '</div>';
    div.innerHTML=html;
    root.appendChild(div);
  }

  const tb=document.getElementById('targetsBody'); tb.innerHTML='';
  for(const t of (state.targets||[])){
    const tr=document.createElement('tr')
    tr.innerHTML='<td>'+esc(t.name)+'</td><td><code>'+esc(t.url)+'</code></td><td>'+esc(t.lastStatus||'pending')+'</td>'+
      '<td>'+(t.id==='local'?'(default)':('<button onclick="delTarget(\''+esc(t.id)+'\')">Remove</button>'))+'</td>'
    tb.appendChild(tr)
  }

  const cb=document.getElementById('consolidatedBody'); cb.innerHTML='';
  const cv = state.consolidatedView || {};
  for(const user of Object.keys(cv).sort()){
    const items = cv[user] || [];
    const text = items.map(x => x.namespace+'/'+x.backend+'/'+x.tool).join('\n');
    const tr=document.createElement('tr');
    tr.innerHTML='<td><b>'+esc(user)+'</b></td><td><pre style="margin:0">'+esc(text || '(none)')+'</pre></td>';
    cb.appendChild(tr);
  }
}

async function loadState(){ try{ renderFromState(await j('/api/state')) }catch(e){ alert(e.message) } }
async function addRepo(){
  try{
    const url=document.getElementById('repoUrl').value;
    const namespace=document.getElementById('repoNs').value;
    renderFromState(await j('/api/admin/repos',{method:'POST',body:JSON.stringify({url,namespace})}));
  }catch(e){ alert('Add repo failed: '+e.message) }
}
async function delRepo(id){
  try{ renderFromState(await j('/api/admin/repos/'+encodeURIComponent(id),{method:'DELETE'})) }
  catch(e){ alert('Remove repo failed: '+e.message)}
}
async function scanAll(){
  try{ renderFromState(await j('/api/admin/scan',{method:'POST',body:'{}'})) }
  catch(e){ alert('Scan failed: '+e.message) }
}
async function addTarget(){
  try{const name=document.getElementById('targetName').value; const url=document.getElementById('targetUrl').value;
  renderFromState(await j('/api/admin/targets',{method:'POST',body:JSON.stringify({name,url})})); document.getElementById('targetName').value=''; document.getElementById('targetUrl').value=''}
  catch(e){ alert('Add target failed: '+e.message)}
}
async function delTarget(id){
  try{ renderFromState(await j('/api/admin/targets/'+encodeURIComponent(id),{method:'DELETE'})) }
  catch(e){ alert('Remove failed: '+e.message)}
}
async function applyPolicy(){
  try{ renderFromState(await j('/api/admin/apply',{method:'POST',body:'{}'})) }
  catch(e){ alert('Apply failed: '+e.message)}
}
async function adminLogin(){
  try{ const token=document.getElementById('adminToken').value; renderFromState(await j('/api/admin/login',{method:'POST',body:JSON.stringify({token})})); await loadState(); }
  catch(e){ alert('Login failed: '+e.message)}
}
async function adminLogout(){
  try{ await j('/api/admin/logout',{method:'POST',body:'{}'}); await loadState(); }
  catch(e){ alert('Logout failed: '+e.message)}
}
loadState();
</script></body></html>`
