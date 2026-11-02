package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxLogLines = 500

// Baked in at build time: -ldflags "-X main.clientVersion=v1.3.0".
var clientVersion = "unknown"

const repoURL = "https://github.com/code2fingers/qwdtt-openwrt"

type statusInfo struct {
	ClientVersion string    `json:"client_version"`
	Mode          string    `json:"mode"`
	Peer          string    `json:"peer"`
	Workers       int       `json:"workers"`
	LastError     string    `json:"last_error"`
	ProxyUp       bool      `json:"proxy_up"`
	Running       bool      `json:"running"`
	Paused        bool      `json:"paused"`
	ExitCode      int       `json:"exit_code"`
	StartedAt     time.Time `json:"started_at"`
	FirstReady    time.Time `json:"first_ready"`
}

var (
	mu         sync.Mutex
	status     statusInfo
	logRing    []string
	args       []string
	clientPath string

	child      *exec.Cmd
	childStdin io.WriteCloser
)

var patterns = []struct {
	re    *regexp.Regexp
	apply func(m []string)
}{
	{regexp.MustCompile(`\[CLIENT\] Mode: (\w+)`), func(m []string) { status.Mode = m[1] }},
	{regexp.MustCompile(`Listening: .* \| Peer: (.*)`), func(m []string) { status.Peer = m[1] }},
	{regexp.MustCompile(`\[DISP\] Worker #\d+ registered \(total: (\d+)\)`), func(m []string) {
		fmt.Sscanf(m[1], "%d", &status.Workers)
		if status.FirstReady.IsZero() {
			status.FirstReady = time.Now()
		}
	}},
	{regexp.MustCompile(`\[WORKER #\d+\] Fatal error: (.*)`), func(m []string) { status.LastError = m[1] }},
	{regexp.MustCompile(`\[SOCKS\] listening`), func(m []string) { status.ProxyUp = true }},
	{regexp.MustCompile(`\[RAW\] TUN attached`), func(m []string) { status.ProxyUp = true }},
}

func handleLine(line string) {
	fmt.Println(line) // keep the container log as it was
	mu.Lock()
	defer mu.Unlock()
	logRing = append(logRing, line)
	if len(logRing) > maxLogLines {
		logRing = logRing[len(logRing)-maxLogLines:]
	}
	for _, p := range patterns {
		if m := p.re.FindStringSubmatch(line); m != nil {
			p.apply(m)
			return
		}
	}
}

func startClient() {
	child = exec.Command(clientPath, args...)
	childStdin, _ = child.StdinPipe()
	outPipe, err := child.StdoutPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "statusd:", err)
		return
	}
	errPipe, err := child.StderrPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "statusd:", err)
		return
	}

	if err := child.Start(); err != nil {
		fmt.Printf("statusd: start: %v\n", err)
		return
	}

	mu.Lock()
	status.Running = true
	status.Paused = false
	status.ExitCode = 0
	status.StartedAt = time.Now()
	status.FirstReady = time.Time{}
	status.Workers = 0
	status.LastError = ""
	status.ProxyUp = false
	mu.Unlock()

	fmt.Printf("statusd: client started (pid %d)\n", child.Process.Pid)

	go func() {
		sc := bufio.NewScanner(outPipe)
		sc.Buffer(make([]byte, 64*1024), 64*1024)
		for sc.Scan() {
			handleLine(sc.Text())
		}
	}()
	go func() {
		sc := bufio.NewScanner(errPipe)
		sc.Buffer(make([]byte, 64*1024), 64*1024)
		for sc.Scan() {
			handleLine(sc.Text())
		}
	}()

	go func() {
		_ = child.Wait()
		mu.Lock()
		status.Running = false
		status.Paused = false
		status.ExitCode = child.ProcessState.ExitCode()
		mu.Unlock()
		fmt.Printf("statusd: client exited (%d)\n", status.ExitCode)
	}()
}

func control(action string) string {
	mu.Lock()
	defer mu.Unlock()
	if action == "start" {
		if status.Running {
			return "already running"
		}
		go startClient()
		return "started"
	}
	if !status.Running {
		return "client is not running"
	}
	switch action {
	case "pause":
		_, _ = io.WriteString(childStdin, "PAUSE\n")
		status.Paused = true
		return "paused"
	case "resume":
		_, _ = io.WriteString(childStdin, "RESUME\n")
		status.Paused = false
		return "resumed"
	case "stop":
		_, _ = io.WriteString(childStdin, "STOP\n")
		return "stop requested"
	}
	return "unknown action"
}

// envConfig builds a client config from QWDTT_* environment variables.
func envConfig() map[string]any {
	cfg := map[string]any{}
	set := func(key, env string) {
		if v := os.Getenv(env); v != "" {
			cfg[key] = v
		}
	}
	set("peer", "QWDTT_PEER")
	if v := os.Getenv("QWDTT_HASHES"); v != "" {
		var hashes []string
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hashes = append(hashes, h)
			}
		}
		cfg["hashes"] = hashes
	}
	set("password", "QWDTT_PASSWORD")
	set("device_id", "QWDTT_DEVICE_ID")
	if _, ok := cfg["device_id"]; !ok {
		// Keep the same default as the flag mode: a config-file build would
		// otherwise use "openwrt" and break the server-side device binding.
		cfg["device_id"] = "unknown"
	}
	set("dns", "QWDTT_DNS")
	if v := os.Getenv("QWDTT_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg["workers"] = n
		}
	}
	return cfg
}

// resolveArgs rewrites the client arguments: if -config points to a file that
// does not exist, or there is no -config at all, build the config from the
// environment. An existing config file always wins.
func resolveArgs() []string {
	cfg := envConfig()
	if len(cfg) <= 1 {
		return args // nothing meaningful in the environment, pass through
	}
	cwd, _ := os.Getwd()
	idx := -1
	for i, a := range args {
		if a == "-config" && i+1 < len(args) {
			idx = i + 1
			break
		}
	}
	if idx >= 0 {
		if _, err := os.Stat(args[idx]); err == nil {
			return args // a real config file exists, use it as is
		}
	}
	path := filepath.Join(cwd, "qwdtt-env.json")
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "statusd: config:", err)
		return args
	}
	_ = json.NewEncoder(f).Encode(cfg)
	_ = f.Close()
	fmt.Printf("statusd: generated config from environment -> %s\n", path)
	if idx >= 0 {
		args[idx] = path
	} else {
		args = append(args, "-config", path)
	}
	return args
}

func main() {
	port := os.Getenv("STATUS_PORT")
	if port == "" {
		port = "8080"
	}
	clientPath = os.Getenv("CLIENT_PATH")
	if clientPath == "" {
		clientPath = "/qwdtt-client"
	}
	args = os.Args[1:]
	args = resolveArgs()

	// SOCKS5 auth via environment: QWDTT_SOCKS_USER / QWDTT_SOCKS_PASS.
	if u, p := os.Getenv("QWDTT_SOCKS_USER"), os.Getenv("QWDTT_SOCKS_PASS"); u != "" && p != "" {
		args = append(args, "-socks-auth", "-socks-user", u, "-socks-pass", p)
		fmt.Printf("statusd: SOCKS5 auth enabled for user %q\n", u)
	}

	// Forward stdin lines to the client (PAUSE/RESUME/STOP).
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			mu.Lock()
			if childStdin != nil {
				_, _ = io.WriteString(childStdin, sc.Text()+"\n")
			}
			mu.Unlock()
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		out := status
		out.ClientVersion = clientVersion
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})

	http.HandleFunc("/api/log", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lines := append([]string{}, logRing...)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, strings.Join(lines, "\n")+"\n")
	})

	http.HandleFunc("/api/control", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		result := control(req.Action)
		fmt.Printf("statusd: control %s -> %s\n", req.Action, result)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"result": result})
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	})

	go func() {
		if err := http.ListenAndServe(":"+port, nil); err != nil {
			fmt.Fprintln(os.Stderr, "statusd:", err)
		}
	}()

	fmt.Printf("statusd: serving on :%s\n", port)
	startClient()

	<-sig
	mu.Lock()
	if status.Running {
		_, _ = io.WriteString(childStdin, "STOP\n")
	}
	mu.Unlock()
	time.Sleep(2 * time.Second)
}

const page = `<!doctype html><html><head><meta charset="utf-8"><title>qwdtt status</title>
<style>body{font-family:monospace;max-width:900px;margin:2rem auto;padding:0 1rem}
pre{background:#111;color:#ddd;padding:1rem;overflow:auto;max-height:60vh}
.k{color:#888}.v{color:#0a0}.bad{color:#c33}
button{font-family:monospace;padding:.4rem .8rem;margin:.2rem .4rem .2rem 0}
</style></head><body>
<h2>qwdtt status</h2>
<div id="s"></div>
<div>
<button onclick="ctl('pause')">Pause</button>
<button onclick="ctl('resume')">Resume</button>
<button onclick="ctl('stop')">Stop</button>
<button onclick="ctl('start')">Start</button>
</div>
<pre id="log"></pre>
<script>
async function ctl(a){await fetch('/api/control',{method:'POST',body:JSON.stringify({action:a})});refresh();}
async function refresh(){
  const r=await fetch('/api/status').then(r=>r.json());
  const up=r.running?(r.paused?'<span class=bad>paused</span>':'<span class=v>running</span>'):'<span class=bad>stopped ('+r.exit_code+')</span>';
  document.getElementById('s').innerHTML=
    '<p><span class=k>state:</span> '+up+
    '<br><span class=k>client:</span> <a href="https://github.com/code2fingers/qwdtt-openwrt">'+(r.client_version||'-')+'</a>'+
    '<br><span class=k>mode:</span> '+(r.mode||'-')+' <span class=k>peer:</span> '+(r.peer||'-')+
    '<br><span class=k>proxy:</span> '+(r.proxy_up?'<span class=v>up</span>':'down')+
    ' <span class=k>workers:</span> '+r.workers+
    '<br><span class=k>connected since:</span> '+(r.first_ready||'-')+
    '<br><span class=k>last error:</span> '+(r.last_error||'-')+'</p>';
  const l=await fetch('/api/log').then(r=>r.text());
  document.getElementById('log').textContent=l;
}
refresh();setInterval(refresh,5000);
</script></body></html>`
