package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

type policyState struct {
	UserAAllowFeedback bool   `json:"userAAllowFeedback"`
	LastApplyStatus    string `json:"lastApplyStatus"`
}

var state = policyState{}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", ui)
	mux.HandleFunc("/api/state", getState)
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
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
	cfg := renderConfig(req.UserAAllowFeedback)
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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &b
	err := cmd.Run()
	return b.String(), err
}

func renderConfig(userAAllowFeedback bool) string {
	userATools := `
    - userId: "user-a"
      tools:
        - backend: "kiwi"
          tool: "search-flight"`
	if userAAllowFeedback {
		userATools = `
    - userId: "user-a"
      tools:
        - backend: "kiwi"
          tool: "search-flight"
        - backend: "kiwi"
          tool: "feedback-to-devs"`
	}

	return `apiVersion: aigateway.envoyproxy.io/v1beta1
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
  userToolPolicies:` + userATools + `
    - userId: "user-b"
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
`
}

const page = `<!doctype html>
<html><head><meta charset="utf-8"><title>AIGW Control Plane UI</title>
<style>body{font-family:Arial;margin:20px;max-width:780px}.card{border:1px solid #ddd;padding:14px;border-radius:8px}button{padding:7px 10px}pre{background:#f7f7f7;padding:10px}</style></head>
<body>
<h2>AIGW Control Plane UI (separate repo)</h2>
<div class="card">
<p>Toggle user-a policy and push config to Docker Envoy AI Gateway.</p>
<label><input id="allow" type="checkbox"/> user-a can access <code>kiwi__feedback-to-devs</code></label><br/><br/>
<button onclick="apply()">Apply To Envoy</button>
<button onclick="loadState()">Refresh State</button>
<pre id="out"></pre>
</div>
<script>
async function loadState(){const r=await fetch('/api/state');document.getElementById('out').textContent=JSON.stringify(await r.json(),null,2)}
async function apply(){const allow=document.getElementById('allow').checked;const r=await fetch('/api/apply',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({userAAllowFeedback:allow})});document.getElementById('out').textContent=await r.text()}
loadState();
</script></body></html>`
