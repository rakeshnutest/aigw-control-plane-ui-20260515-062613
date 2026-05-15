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

type skillTool struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type skillEntry struct {
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	Backend string      `json:"backend"`
	Tools   []skillTool `json:"tools"`
}

type policyState struct {
	RepoURL         string        `json:"repoUrl"`
	SkillFiles      []string      `json:"skillFiles"`
	ScannedTools    []string      `json:"scannedTools"`
	Skills          []skillEntry  `json:"skills"`
	Targets         []envoyTarget `json:"targets"`
	LastScanStatus  string        `json:"lastScanStatus"`
	LastApplyStatus string        `json:"lastApplyStatus"`
}

type applyPayload struct {
	ConfigYAML string `json:"configYaml"`
}

var (
	mu sync.RWMutex

	state = policyState{
		Skills: seededSkills(),
		Targets: []envoyTarget{ {
			ID:   "local",
			Name: "Local Envoy (Docker)",
			URL:  "http://127.0.0.1:18081/adapter/policy/apply",
		}},
	}
)

func seededSkills() []skillEntry {
	return []skillEntry{
		{ID: "skill-kiwi", Name: "Travel Search (Kiwi)", Backend: "kiwi", Tools: []skillTool{{Name: "search-flight", Enabled: true}, {Name: "feedback-to-devs", Enabled: false}}},
		{ID: "skill-github", Name: "GitHub Pull Requests", Backend: "skill-github", Tools: []skillTool{{Name: "pull_request_read", Enabled: true}, {Name: "pull_request_list", Enabled: true}}},
		{ID: "skill-jira", Name: "Jira Issues", Backend: "skill-jira", Tools: []skillTool{{Name: "issue_get", Enabled: true}, {Name: "issue_search", Enabled: true}}},
		{ID: "skill-slack", Name: "Slack Search", Backend: "skill-slack", Tools: []skillTool{{Name: "channel_search", Enabled: true}, {Name: "thread_read", Enabled: false}}},
		{ID: "skill-wiki", Name: "Internal Wiki", Backend: "skill-wiki", Tools: []skillTool{{Name: "wiki_search", Enabled: true}, {Name: "wiki_read", Enabled: true}}},
	}
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", ui)
	mux.HandleFunc("/api/state", getState)
	mux.HandleFunc("/api/scan", scanRepo)
	mux.HandleFunc("/api/targets", targets)
	mux.HandleFunc("/api/targets/", deleteTarget)
	mux.HandleFunc("/api/skills/toggle", toggleTool)
	mux.HandleFunc("/api/apply", apply)
	mux.HandleFunc("/adapter/policy/apply", localAdapterApply)

	addr := ":18081"
	log.Printf("control plane UI listening at http://127.0.0.1%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func ui(w http.ResponseWriter, _ *http.Request) {
	tmpl := template.Must(template.New("ui").Parse(page))
	_ = tmpl.Execute(w, nil)
}

func getState(w http.ResponseWriter, _ *http.Request) {
	mu.RLock()
	defer mu.RUnlock()
	writeJSON(w, state)
}

func scanRepo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct{ RepoURL string `json:"repoUrl"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	owner, repo, err := parseGitHubRepo(req.RepoURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	branch, err := fetchDefaultBranch(owner, repo)
	if err != nil {
		http.Error(w, fmt.Sprintf("default branch fetch failed: %v", err), http.StatusBadGateway)
		return
	}
	files, err := fetchSkillFiles(owner, repo, branch)
	if err != nil {
		http.Error(w, fmt.Sprintf("repo tree scan failed: %v", err), http.StatusBadGateway)
		return
	}
	tools := map[string]struct{}{}
	for _, f := range files {
		t, err := fetchToolsFromSkillFile(owner, repo, branch, f)
		if err != nil {
			continue
		}
		for _, tool := range t {
			tools[tool] = struct{}{}
		}
	}
	toolList := make([]string, 0, len(tools))
	for t := range tools {
		toolList = append(toolList, t)
	}
	sort.Strings(toolList)
	sort.Strings(files)

	mu.Lock()
	state.RepoURL = req.RepoURL
	state.SkillFiles = files
	state.ScannedTools = toolList
	state.LastScanStatus = fmt.Sprintf("Scanned %d SKILL.md file(s) from %s/%s@%s", len(files), owner, repo, branch)
	mu.Unlock()
	getState(w, r)
}

func targets(w http.ResponseWriter, r *http.Request) {
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

func deleteTarget(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/targets/")
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

func toggleTool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SkillID string `json:"skillId"`
		Tool    string `json:"tool"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	for i := range state.Skills {
		if state.Skills[i].ID != req.SkillID {
			continue
		}
		for j := range state.Skills[i].Tools {
			if state.Skills[i].Tools[j].Name == req.Tool {
				state.Skills[i].Tools[j].Enabled = req.Enabled
				writeJSON(w, state)
				return
			}
		}
	}
	http.Error(w, "skill/tool not found", http.StatusNotFound)
}

func apply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mu.RLock()
	skills := append([]skillEntry(nil), state.Skills...)
	targets := append([]envoyTarget(nil), state.Targets...)
	mu.RUnlock()

	cfg := renderConfig(skills)
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
	mu.Unlock()
	getState(w, r)
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

func fetchToolsFromSkillFile(owner, repo, branch, path string) ([]string, error) {
	rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", owner, repo, branch, path)
	req, _ := http.NewRequest(http.MethodGet, rawURL, nil)
	req.Header.Set("User-Agent", "aigw-control-plane-ui")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	content := b.String()
	r := regexp.MustCompile(`(?m)^\s*-\s*([A-Za-z0-9_\-\.]+)\s*$`)
	m := r.FindAllStringSubmatch(content, -1)
	tools := []string{}
	for _, mm := range m {
		t := strings.TrimSpace(mm[1])
		if t != "" {
			tools = append(tools, t)
		}
	}
	return tools, nil
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

func renderConfig(skills []skillEntry) string {
	userAToolsByBackend := map[string][]string{}
	for _, sk := range skills {
		for _, t := range sk.Tools {
			if t.Enabled {
				userAToolsByBackend[sk.Backend] = append(userAToolsByBackend[sk.Backend], t.Name)
			}
		}
	}
	for k := range userAToolsByBackend {
		sort.Strings(userAToolsByBackend[k])
	}
	backendNames := []string{"kiwi", "skill-github", "skill-jira", "skill-slack", "skill-wiki"}

	var b strings.Builder
	b.WriteString(`apiVersion: aigateway.envoyproxy.io/v1beta1
kind: MCPRoute
metadata:
  name: mcp-main
  namespace: default
spec:
  parentRefs:
    - name: aigw-run
      kind: Gateway
      group: gateway.networking.k8s.io
  path: "/mcp"
  backendRefs:
`)
	for _, backend := range backendNames {
		b.WriteString("    - name: \"")
		b.WriteString(backend)
		b.WriteString("\"\n")
		b.WriteString("      kind: Backend\n")
		b.WriteString("      group: gateway.envoyproxy.io\n")
		b.WriteString("      path: \"/\"\n")
	}

	b.WriteString("  userToolPolicies:\n")
	b.WriteString("    - userId: \"user-a\"\n")
	b.WriteString("      tools:\n")
	for backend, tools := range userAToolsByBackend {
		for _, tool := range tools {
			b.WriteString("        - backend: \"")
			b.WriteString(backend)
			b.WriteString("\"\n")
			b.WriteString("          tool: \"")
			b.WriteString(tool)
			b.WriteString("\"\n")
		}
	}
	b.WriteString(`    - userId: "user-b"
      allowAll: true
    - userId: "*"
      tools:
        - backend: "kiwi"
          tool: "search-flight"
---
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
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: Backend
metadata:
  name: skill-github
  namespace: default
spec:
  endpoints:
    - fqdn:
        hostname: mcp.kiwi.com
        port: 443
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: Backend
metadata:
  name: skill-jira
  namespace: default
spec:
  endpoints:
    - fqdn:
        hostname: mcp.kiwi.com
        port: 443
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: Backend
metadata:
  name: skill-slack
  namespace: default
spec:
  endpoints:
    - fqdn:
        hostname: mcp.kiwi.com
        port: 443
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: Backend
metadata:
  name: skill-wiki
  namespace: default
spec:
  endpoints:
    - fqdn:
        hostname: mcp.kiwi.com
        port: 443
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
body{font-family:Arial;margin:20px;max-width:1200px;background:#fafafa}
.card{border:1px solid #ddd;padding:14px;border-radius:8px;margin-bottom:14px;background:#fff}
button{padding:7px 10px;margin-right:6px}
input{padding:6px}
input[type=text]{width:460px}
pre{background:#f7f7f7;padding:10px;white-space:pre-wrap}
code{background:#f0f0f0;padding:2px 4px;border-radius:4px}
table{width:100%;border-collapse:collapse;margin-top:8px}
th,td{border:1px solid #ddd;padding:8px;text-align:left}
th{background:#fafafa}
.badge{display:inline-block;padding:2px 8px;border-radius:999px;background:#eef}
.skills{display:grid;grid-template-columns:repeat(2,minmax(300px,1fr));gap:12px}
.skill{border:1px solid #e5e5e5;border-radius:8px;padding:10px;background:#fcfcfc}
.skill h4{margin:0 0 6px 0}
small{color:#555}
</style></head>
<body>
<h2>AIGW Control Plane UI</h2>

<div class="card">
  <h3>1) Scan GitHub repo</h3>
  <input id="repo" type="text" placeholder="https://github.com/org/repo" value="https://github.com/rakeshnutest/skill-e2e-demo-20260515-060359"/>
  <button onclick="scanRepo()">Scan</button>
  <div id="scanStatus" style="margin-top:8px"></div>
</div>

<div class="card">
  <h3>2) Skill catalog (enable/disable tools)</h3>
  <div class="skills" id="skills"></div>
</div>

<div class="card">
  <h3>3) Envoy targets</h3>
  <div>
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
  <h3>4) Apply policy</h3>
  <button onclick="applyPolicy()">Push Policy To All Targets</button>
  <button onclick="loadState()">Refresh</button>
</div>

<div class="card">
  <h3>State</h3>
  <pre id="out"></pre>
</div>

<script>
function esc(s){return String(s??'').replace(/[&<>"']/g,m=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[m]))}
async function j(url,opts={}){const r=await fetch(url,Object.assign({headers:{'content-type':'application/json'}},opts));const t=await r.text();if(!r.ok) throw new Error(t);return t?JSON.parse(t):{}}

async function toggleTool(skillId,tool,enabled){
  try{ render(await j('/api/skills/toggle',{method:'POST',body:JSON.stringify({skillId,tool,enabled})})) }
  catch(e){ alert('Toggle failed: '+e.message) }
}

function renderSkills(skills){
  const root=document.getElementById('skills'); root.innerHTML='';
  for(const sk of (skills||[])){
    const div=document.createElement('div'); div.className='skill';
    let html='<h4>'+esc(sk.name)+'</h4><small>backend: <code>'+esc(sk.backend)+'</code></small><div style="margin-top:8px">';
    for(const t of (sk.tools||[])){
      const checked=t.enabled?'checked':'';
      html += '<label style="display:block;margin-bottom:4px"><input type="checkbox" '+checked+' onchange="toggleTool(\''+esc(sk.id)+'\',\''+esc(t.name)+'\',this.checked)"/> <code>'+esc(t.name)+'</code></label>';
    }
    html += '</div>';
    div.innerHTML=html;
    root.appendChild(div);
  }
}

function render(state){
  document.getElementById('out').textContent = JSON.stringify(state,null,2)
  document.getElementById('scanStatus').innerHTML = '<span class="badge">'+esc(state.lastScanStatus||'No scan yet')+'</span>'
  renderSkills(state.skills)

  const tb=document.getElementById('targetsBody')
  tb.innerHTML=''
  for(const t of (state.targets||[])){
    const tr=document.createElement('tr')
    tr.innerHTML='<td>'+esc(t.name)+'</td><td><code>'+esc(t.url)+'</code></td><td>'+esc(t.lastStatus||'pending')+'</td>'+
      '<td>'+(t.id==='local'?'(default)':('<button onclick="delTarget(\''+esc(t.id)+'\')">Remove</button>'))+'</td>'
    tb.appendChild(tr)
  }
}

async function loadState(){ try{ render(await j('/api/state')) }catch(e){ alert(e.message) } }
async function scanRepo(){
  try{ const repoUrl=document.getElementById('repo').value; render(await j('/api/scan',{method:'POST',body:JSON.stringify({repoUrl})})) }
  catch(e){ alert('Scan failed: '+e.message) }
}
async function addTarget(){
  try{const name=document.getElementById('targetName').value; const url=document.getElementById('targetUrl').value;
  render(await j('/api/targets',{method:'POST',body:JSON.stringify({name,url})})); document.getElementById('targetName').value=''; document.getElementById('targetUrl').value=''}
  catch(e){ alert('Add target failed: '+e.message)}
}
async function delTarget(id){
  try{ render(await j('/api/targets/'+encodeURIComponent(id),{method:'DELETE'})) }
  catch(e){ alert('Remove failed: '+e.message)}
}
async function applyPolicy(){
  try{ render(await j('/api/apply',{method:'POST',body:'{}'})) }
  catch(e){ alert('Apply failed: '+e.message)}
}
loadState();
</script></body></html>`
