package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	HTTPProxy     string    `json:"http_proxy"`
	Running       bool      `json:"running"`
	Paused        bool      `json:"paused"`
	ExitCode      int       `json:"exit_code"`
	StartedAt     time.Time `json:"started_at"`
	FirstReady    time.Time `json:"first_ready"`
}

var (
	mu           sync.Mutex
	status       statusInfo
	logRing      []string
	args         []string
	clientPath   string
	shuttingDown bool

	child      *exec.Cmd
	childStdin io.WriteCloser
)

// notify runs the notifier script (NOTIFY_CMD, default /usr/local/bin/notify)
// with the message as its argument. NOTIFY_CMD=none disables notifications.
// The script itself chooses the backend (ntfy / Telegram / Gotify / ...), so the
// delivery method can be changed without touching statusd.
func notify(msg string) {
	cmd := os.Getenv("NOTIFY_CMD")
	if cmd == "none" {
		return
	}
	if cmd == "" {
		cmd = "/usr/local/bin/notify"
	}
	c := exec.Command(cmd, msg)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "statusd: notify:", err)
	}
}

// socksConn opens a connection to target through the SOCKS5 server at proxy.
func socksConn(proxy, user, pass, target string) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", proxy, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if _, err := c.Write([]byte{5, 2, 0, 1}); err != nil {
		c.Close()
		return nil, err
	}
	greet := make([]byte, 2)
	if _, err := io.ReadFull(c, greet); err != nil {
		c.Close()
		return nil, err
	}
	if greet[0] != 5 {
		c.Close()
		return nil, fmt.Errorf("socks: unexpected greeting version %d", greet[0])
	}
	if greet[1] == 2 {
		if user == "" || pass == "" {
			c.Close()
			return nil, fmt.Errorf("socks: username and password required")
		}
		req := append([]byte{1, byte(len(user))}, user...)
		req = append(req, byte(len(pass)))
		req = append(req, pass...)
		if _, err := c.Write(req); err != nil {
			c.Close()
			return nil, err
		}
		st := make([]byte, 2)
		if _, err := io.ReadFull(c, st); err != nil {
			c.Close()
			return nil, err
		}
		if st[1] != 0 {
			c.Close()
			return nil, fmt.Errorf("socks: authentication failed")
		}
	} else if greet[1] != 0 {
		c.Close()
		return nil, fmt.Errorf("socks: no acceptable method (%d)", greet[1])
	}

	host, port, err := net.SplitHostPort(target)
	if err != nil {
		c.Close()
		return nil, err
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		c.Close()
		return nil, err
	}
	req := []byte{5, 1, 0}
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		req = append(req, 1)
		req = append(req, ip.To4()...)
	} else {
		req = append(req, 3, byte(len(host)))
		req = append(req, host...)
	}
	req = append(req, byte(p>>8), byte(p&0xff))
	if _, err := c.Write(req); err != nil {
		c.Close()
		return nil, err
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c, hdr); err != nil {
		c.Close()
		return nil, err
	}
	if hdr[1] != 0 {
		c.Close()
		return nil, fmt.Errorf("socks: connect rejected (%d)", hdr[1])
	}
	// Skip the bound address; the caller only needs the tunnel.
	switch hdr[3] {
	case 1:
		_, _ = io.CopyN(io.Discard, c, 6)
	case 3:
		n := make([]byte, 1)
		if _, err := io.ReadFull(c, n); err != nil {
			c.Close()
			return nil, err
		}
		_, _ = io.CopyN(io.Discard, c, int64(n[0])+2)
	case 4:
		_, _ = io.CopyN(io.Discard, c, 18)
	}
	return c, nil
}

// startHTTPProxy serves an HTTP proxy in front of the client's SOCKS5 server
// (the client itself has no HTTP mode). Enabled when HTTP_PROXY_PORT is set.
// CONNECT requests are tunneled raw; absolute-URI requests go through the SOCKS dialer.
func startHTTPProxy() {
	port := os.Getenv("HTTP_PROXY_PORT")
	if port == "" {
		return
	}
	upstream := os.Getenv("SOCKS_UPSTREAM") // the client's SOCKS5 address
	if upstream == "" {
		upstream = "127.0.0.1:1080"
	}
	user, pass := os.Getenv("QWDTT_SOCKS_USER"), os.Getenv("QWDTT_SOCKS_PASS")

	transport := &http.Transport{
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			return socksConn(upstream, user, pass, addr)
		},
	}
	client := &http.Client{Transport: transport}

	mu.Lock()
	status.HTTPProxy = ":" + port
	mu.Unlock()

	fmt.Printf("statusd: http proxy on :%s -> socks %s\n", port, upstream)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			conn, err := socksConn(upstream, user, pass, r.Host)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer conn.Close()
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "CONNECT not supported", http.StatusInternalServerError)
				return
			}
			client, _, err := hj.Hijack()
			if err != nil {
				return
			}
			defer client.Close()
			fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
			go func() {
				_, _ = io.Copy(conn, client)
				conn.Close()
			}()
			_, _ = io.Copy(client, conn)
			return
		}
		if r.URL.Scheme == "" || r.URL.Host == "" {
			http.Error(w, "expected an absolute request URI", http.StatusBadRequest)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
	if err := http.ListenAndServe(":"+port, handler); err != nil {
		fmt.Fprintln(os.Stderr, "statusd: http proxy:", err)
	}
}

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
	logRing = append(logRing, line)
	if len(logRing) > maxLogLines {
		logRing = logRing[len(logRing)-maxLogLines:]
	}
	var msg string
	for _, p := range patterns {
		if m := p.re.FindStringSubmatch(line); m != nil {
			wasUp := status.ProxyUp
			p.apply(m)
			if !wasUp && status.ProxyUp {
				msg = fmt.Sprintf("qwdtt-socks: proxy is up (peer %s)", status.Peer)
			}
			break
		}
	}
	mu.Unlock()
	if msg != "" {
		notify(msg)
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
		shut := shuttingDown
		mu.Unlock()
		fmt.Printf("statusd: client exited (%d)\n", status.ExitCode)
		if !shut {
			notify(fmt.Sprintf("qwdtt-socks: proxy is down (exit %d)", status.ExitCode))
		}
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

	go startHTTPProxy()

	fmt.Printf("statusd: serving on :%s\n", port)
	startClient()

	<-sig
	mu.Lock()
	shuttingDown = true
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
    '<br><span class=k>http proxy:</span> '+(r.http_proxy||'-')+
    '<br><span class=k>connected since:</span> '+(r.first_ready||'-')+
    '<br><span class=k>last error:</span> '+(r.last_error||'-')+'</p>';
  const l=await fetch('/api/log').then(r=>r.text());
  document.getElementById('log').textContent=l;
}
refresh();setInterval(refresh,5000);
</script></body></html>`
