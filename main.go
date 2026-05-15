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
)

type policyState struct {
	UserAAllowFeedback bool     `json:"userAAllowFeedback"`
	RepoURL            string   `json:"repoUrl"`
	SkillFiles         []string `json:"skillFiles"`
	ScannedTools       []string `json:"scannedTools"`
	LastScanStatus     string   `json:"lastScanStatus"`
	LastApplyStatus    string   `json:"lastApplyStatus"`
}

var state = policyState{}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", ui)
	mux.HandleFunc("/api/state", getState)
	mux.HandleFunc("/api/scan", scanRepo)
	mux.HandleFunc("/api/apply", apply)

	addr := ":18081"
	log.Printf("control plane UI listening at http://127.0.0.1%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func ui(w http.ResponseWriter, _ *http.Request) {
	tmpl := template.Must(template.New("ui").Parse(page))
	_ = tmpl.Execute(w, nil)
}

func getState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, state)
}

func scanRepo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		RepoURL string `json:"repoUrl"`
	}
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

	state.RepoURL = req.RepoURL
	state.SkillFiles = files
	state.ScannedTools = toolList
	state.LastScanStatus = fmt.Sprintf("scanned %d SKILL.md files from %s/%s@%s", len(files), owner, repo, branch)

	writeJSON(w, state)
}

func apply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		UserAAllowFeedback bool `json:"userAAllowFeedback"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	cfgPath := "/tmp/aigw-dyn/config.yaml"
	if err := os.MkdirAll("/tmp/aigw-dyn", 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cfg := renderConfig(req.UserAAllowFeedback, state.ScannedTools)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if out, err := run("docker", "rm", "-f", "aigw-mcp-test"); err != nil {
		if !strings.Contains(out, "No such container") {
			http.Error(w, fmt.Sprintf("failed to remove old container: %v: %s", err, out), http.StatusInternalServerError)
			return
		}
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

	state.UserAAllowFeedback = req.UserAAllowFeedback
	state.LastApplyStatus = "applied: " + strings.TrimSpace(out)
	writeJSON(w, state)
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
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo)
	var body struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := getJSON(url, &body); err != nil {
		return "", err
	}
	if body.DefaultBranch == "" {
		return "", fmt.Errorf("default branch is empty")
	}
	return body.DefaultBranch, nil
}

func fetchSkillFiles(owner, repo, branch string) ([]string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/trees/%s?recursive=1", owner, repo, branch)
	var body struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err := getJSON(url, &body); err != nil {
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
		if t == "" {
			continue
		}
		tools = append(tools, t)
	}
	return tools, nil
}

func getJSON(url string, out any) error {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
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

func renderConfig(userAAllowFeedback bool, scannedTools []string) string {
	// Keep deterministic baseline tools for demo backend.
	userATools := []string{"search-flight"}
	if userAAllowFeedback {
		userATools = append(userATools, "feedback-to-devs")
	}
	// If scan discovered exactly these kiwi tool names, keep them in sync.
	for _, t := range scannedTools {
		if t == "search-flight" || t == "feedback-to-devs" {
			if !contains(userATools, t) {
				userATools = append(userATools, t)
			}
		}
	}
	sort.Strings(userATools)

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
    - name: kiwi
      kind: Backend
      group: gateway.envoyproxy.io
      path: "/"
  userToolPolicies:
    - userId: "user-a"
      tools:
`)
	for _, t := range userATools {
		b.WriteString("        - backend: \"kiwi\"\n")
		b.WriteString("          tool: \"")
		b.WriteString(t)
		b.WriteString("\"\n")
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

func contains(arr []string, v string) bool {
	for _, x := range arr {
		if x == v {
			return true
		}
	}
	return false
}

const page = `<!doctype html>
<html><head><meta charset="utf-8"><title>AIGW Control Plane UI</title>
<style>
body{font-family:Arial;margin:20px;max-width:900px}
.card{border:1px solid #ddd;padding:14px;border-radius:8px;margin-bottom:12px}
button{padding:7px 10px;margin-right:6px}
input{padding:6px;width:560px}
pre{background:#f7f7f7;padding:10px;white-space:pre-wrap}
code{background:#f0f0f0;padding:2px 4px;border-radius:4px}
</style></head>
<body>
<h2>AIGW Control Plane UI (separate repo)</h2>
<div class="card">
  <h3>1) Scan GitHub repo</h3>
  <input id="repo" value="https://github.com/rakeshnutest/skill-e2e-demo-20260515-060359"/>
  <button onclick="scanRepo()">Scan</button>
  <div><small>Finds <code>SKILL.md</code> files and extracts bullet-list tools.</small></div>
</div>
<div class="card">
  <h3>2) Apply policy to Envoy</h3>
  <label><input id="allow" type="checkbox"/> user-a can access <code>kiwi__feedback-to-devs</code></label><br/><br/>
  <button onclick="apply()">Apply To Envoy</button>
  <button onclick="loadState()">Refresh State</button>
</div>
<pre id="out"></pre>
<script>
async function loadState(){
  const r=await fetch('/api/state');
  document.getElementById('out').textContent=JSON.stringify(await r.json(),null,2)
}
async function scanRepo(){
  const repoUrl=document.getElementById('repo').value;
  const r=await fetch('/api/scan',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({repoUrl})});
  document.getElementById('out').textContent=await r.text();
}
async function apply(){
  const allow=document.getElementById('allow').checked;
  const r=await fetch('/api/apply',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({userAAllowFeedback:allow})});
  document.getElementById('out').textContent=await r.text();
}
loadState();
</script></body></html>`
