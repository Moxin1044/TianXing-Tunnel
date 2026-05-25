package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
)

//go:embed static/bootstrap.min.css
var bootstrapCSSData []byte

//go:embed static/bootstrap.bundle.min.js
var bootstrapJSData []byte

var (
	Version   = "1.0.0"
	BuildTime = "unknown"
	GoVersion = "unknown"
)

// ==================== Logging ====================

func getLogDir() string {
	switch runtime.GOOS {
	case "windows":
		dir := os.Getenv("LOCALAPPDATA")
		if dir == "" {
			dir = os.Getenv("APPDATA")
		}
		if dir != "" {
			return filepath.Join(dir, "TianXing")
		}
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Logs", "TianXing")
	default:
		dir := os.Getenv("XDG_STATE_HOME")
		if dir == "" {
			home, _ := os.UserHomeDir()
			dir = filepath.Join(home, ".local", "state")
		}
		return filepath.Join(dir, "tianxing")
	}
	return "."
}

func setupLogging() {
	logDir := getLogDir()
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Printf("[WARN] Failed to create log directory %s: %v", logDir, err)
		return
	}
	logPath := filepath.Join(logDir, "tianxing.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("[WARN] Failed to open log file %s: %v", logPath, err)
		return
	}
	mw := io.MultiWriter(os.Stderr, f)
	log.SetOutput(mw)
	gin.DefaultWriter = mw
	gin.DefaultErrorWriter = mw
	log.Printf("[TianXing] Log file: %s", logPath)
}

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// ==================== Config ====================

type ServerConfig struct {
	BindAddr string   `json:"bind_addr"`
	BindPort int      `json:"bind_port"`
	WebPort  int      `json:"web_port"`
	WebUser  string   `json:"web_user"`
	WebPass  string   `json:"web_pass"`
	Tokens   []string `json:"tokens"`
}

func loadServerConfig(path string) *ServerConfig {
	cfg := &ServerConfig{
		BindAddr: "0.0.0.0",
		BindPort: 7000,
		WebPort:  4200,
		Tokens:   []string{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		switch key {
		case "bind_addr":
			cfg.BindAddr = val
		case "bind_port":
			cfg.BindPort, _ = strconv.Atoi(val)
		case "web_port":
			cfg.WebPort, _ = strconv.Atoi(val)
		case "token":
			cfg.Tokens = append(cfg.Tokens, val)
		case "web_user":
			cfg.WebUser = val
		case "web_pass":
			cfg.WebPass = val
		}
	}
	if len(cfg.Tokens) == 0 {
		cfg.Tokens = append(cfg.Tokens, "tianxing_default_token")
		log.Println("[WARN] No token configured, using default token: tianxing_default_token")
	}
	return cfg
}

// ==================== Tunnel Mapping ====================

type TunnelMapping struct {
	ID         int    `json:"id"`
	ClientName string `json:"client_name"`
	Type       string `json:"type"` // tcp, udp
	RemotePort int    `json:"remote_port"`
	LocalIP    string `json:"local_ip"`
	LocalPort  int    `json:"local_port"`
	Status     string `json:"status"` // active, paused
}

// ==================== Client Connection ====================

type ClientConn struct {
	Name          string
	Token         string
	Conn          net.Conn
	Writer        *bufio.Writer
	RemoteIP      string
	Connected     time.Time
	LastHeartbeat time.Time
	mu            sync.Mutex
}

func (c *ClientConn) SendMsg(msg string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := c.Writer.WriteString(msg + "\n")
	if err != nil {
		return err
	}
	return c.Writer.Flush()
}

// ==================== Server Core ====================

type Server struct {
	cfg       *ServerConfig
	clients   map[string]*ClientConn // name -> client
	tunnels   map[int]*TunnelMapping // remote_port -> mapping
	listeners map[int]net.Listener   // remote_port -> listener
	workConns map[string]net.Conn    // work_id -> proxy conn
	workQueue map[string]net.Conn    // work_id -> incoming conn
	mu        sync.RWMutex
	nextID    int
	stats     Stats
	logs      []string
	logMu     sync.RWMutex
}

type Stats struct {
	TotalConnections int64 `json:"total_connections"`
	TotalTraffic     int64 `json:"total_traffic"`
	ActiveConns      int64 `json:"active_conns"`
}

func NewServer(cfg *ServerConfig) *Server {
	return &Server{
		cfg:       cfg,
		clients:   make(map[string]*ClientConn),
		tunnels:   make(map[int]*TunnelMapping),
		listeners: make(map[int]net.Listener),
		workConns: make(map[string]net.Conn),
		workQueue: make(map[string]net.Conn),
		logs:      make([]string, 0, 200),
	}
}

func (s *Server) addLog(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	now := time.Now().Format("15:04:05")
	entry := fmt.Sprintf("[%s] %s", now, msg)
	s.logMu.Lock()
	if len(s.logs) >= 200 {
		copy(s.logs, s.logs[1:])
		s.logs[199] = entry
	} else {
		s.logs = append(s.logs, entry)
	}
	s.logMu.Unlock()
	log.Printf("%s", msg)
}

func (s *Server) validateToken(token string) bool {
	for _, t := range s.cfg.Tokens {
		if t == token {
			return true
		}
	}
	return false
}

// ==================== Control Connection Handler ====================

func (s *Server) startControlServer() {
	addr := fmt.Sprintf("%s:%d", s.cfg.BindAddr, s.cfg.BindPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("[Control] Listen failed: %v", err)
	}
	log.Printf("[Control] Listening on %s", addr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("[Control] Accept error: %v", err)
			continue
		}
		go s.handleControlConn(conn)
	}
}

func (s *Server) handleControlConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	// AUTH
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	line = strings.TrimSpace(line)
	parts := strings.SplitN(line, " ", 3) // AUTH <token> <name>
	if len(parts) < 2 || parts[0] != "AUTH" {
		writer.WriteString("AUTH_FAILED invalid_command\n")
		writer.Flush()
		return
	}

	token := parts[1]
	clientName := "anonymous"
	if len(parts) >= 3 {
		clientName = parts[2]
	}

	if !s.validateToken(token) {
		writer.WriteString("AUTH_FAILED invalid_token\n")
		writer.Flush()
		return
	}

	writer.WriteString("OK\n")
	writer.Flush()
	conn.SetDeadline(time.Time{})

	host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	client := &ClientConn{
		Name:          clientName,
		Token:         token,
		Conn:          conn,
		Writer:        writer,
		RemoteIP:      host,
		Connected:     time.Now(),
		LastHeartbeat: time.Now(),
	}

	s.mu.Lock()
	// Close old connection if same-name client exists
	if old, ok := s.clients[clientName]; ok {
		old.Conn.Close()
		delete(s.clients, clientName)
	}
	s.clients[clientName] = client
	s.mu.Unlock()

	s.addLog("[Control] Client '%s' connected from %s", clientName, host)

	// Heartbeat and command loop
	for {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		cmd := strings.TrimSpace(line)
		s.handleClientCommand(client, cmd)
	}

	s.mu.Lock()
	delete(s.clients, clientName)
	// Remove all tunnels for this client
	portsToRemove := make([]int, 0)
	for port, t := range s.tunnels {
		if t.ClientName == clientName {
			portsToRemove = append(portsToRemove, port)
		}
	}
	for _, port := range portsToRemove {
		if ln, ok := s.listeners[port]; ok {
			ln.Close()
			delete(s.listeners, port)
		}
		delete(s.tunnels, port)
	}
	s.mu.Unlock()
	s.addLog("[Control] Client '%s' disconnected, removed %d tunnel(s)", clientName, len(portsToRemove))
}

func (s *Server) handleClientCommand(client *ClientConn, cmd string) {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return
	}

	switch fields[0] {
	case "PING":
		client.LastHeartbeat = time.Now()
		client.SendMsg("PONG")

	case "ADD":
		// ADD <type> <remote_port> <local_ip> <local_port>
		if len(fields) < 5 {
			client.SendMsg("ADD_FAILED invalid_command")
			return
		}
		tunnelType := fields[1]
		remotePort, _ := strconv.Atoi(fields[2])
		localIP := fields[3]
		localPort, _ := strconv.Atoi(fields[4])

		err := s.createTunnel(client.Name, tunnelType, remotePort, localIP, localPort)
		if err != nil {
			client.SendMsg(fmt.Sprintf("ADD_FAILED %s", err.Error()))
		} else {
			client.SendMsg(fmt.Sprintf("ADD_OK %d", remotePort))
		}

	case "ADD_RANGE":
		// ADD_RANGE <type> <port_start>-<port_end> <local_ip>
		if len(fields) < 4 {
			client.SendMsg("ADD_FAILED invalid_command")
			return
		}
		tunnelType := fields[1]
		portRange := fields[2]
		localIP := fields[3]

		start, end, err := parsePortRange(portRange)
		if err != nil {
			client.SendMsg(fmt.Sprintf("ADD_FAILED %s", err.Error()))
			return
		}

		created := 0
		for port := start; port <= end; port++ {
			if e := s.createTunnel(client.Name, tunnelType, port, localIP, port); e != nil {
				client.SendMsg(fmt.Sprintf("ADD_RANGE_PARTIAL %d-%d created=%d error=%s", start, end, created, e.Error()))
				return
			}
			created++
		}
		client.SendMsg(fmt.Sprintf("ADD_RANGE_OK %d-%d count=%d", start, end, created))

	case "REMOVE":
		// REMOVE <remote_port>
		if len(fields) < 2 {
			client.SendMsg("REMOVE_FAILED invalid_command")
			return
		}
		remotePort, _ := strconv.Atoi(fields[1])
		if err := s.removeTunnel(remotePort); err != nil {
			client.SendMsg(fmt.Sprintf("REMOVE_FAILED %s", err.Error()))
		} else {
			client.SendMsg("REMOVE_OK")
		}

	case "LIST":
		s.mu.RLock()
		list := make([]TunnelMapping, 0)
		for _, t := range s.tunnels {
			if t.ClientName == client.Name {
				list = append(list, *t)
			}
		}
		s.mu.RUnlock()
		data, _ := json.Marshal(list)
		client.SendMsg(string(data))

	case "WORK":
		// WORK <work_id> - Client establishes work connection
		if len(fields) < 2 {
			return
		}
		workID := fields[1]
		s.mu.Lock()
		incoming, ok := s.workQueue[workID]
		if ok {
			delete(s.workQueue, workID)
			s.workConns[workID] = connFromClient(client.Conn)
			s.mu.Unlock()
			// Bridge incoming and client work connection
			go s.bridgeConnection(incoming, client.Conn, workID)
		} else {
			s.workConns[workID] = client.Conn
			s.mu.Unlock()
		}
	}
}

func connFromClient(c net.Conn) net.Conn { return c }

func (s *Server) bridgeConnection(incoming, workConn net.Conn, workID string) {
	atomic.AddInt64(&s.stats.ActiveConns, 1)
	atomic.AddInt64(&s.stats.TotalConnections, 1)

	defer func() {
		atomic.AddInt64(&s.stats.ActiveConns, -1)
		incoming.Close()
		s.mu.Lock()
		delete(s.workConns, workID)
		s.mu.Unlock()
	}()

	var traffic int64
	done := make(chan struct{}, 2)

	// Use 64KB buffer for high-throughput data transfer
	buf := make([]byte, 64*1024)

	copy := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		n, _ := io.CopyBuffer(dst, src, buf)
		atomic.AddInt64(&traffic, n)
		dst.Close()
	}

	go copy(incoming, workConn)
	go copy(workConn, incoming)

	<-done
	atomic.AddInt64(&s.stats.TotalTraffic, traffic)
}

// ==================== Tunnel Management ====================

func (s *Server) createTunnel(clientName, tunnelType string, remotePort int, localIP string, localPort int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.tunnels[remotePort]; exists {
		return fmt.Errorf("port %d already in use", remotePort)
	}

	// Listen on port
	addr := fmt.Sprintf("%s:%d", s.cfg.BindAddr, remotePort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen failed: %v", err)
	}

	mapping := &TunnelMapping{
		ID:         s.nextID,
		ClientName: clientName,
		Type:       tunnelType,
		RemotePort: remotePort,
		LocalIP:    localIP,
		LocalPort:  localPort,
		Status:     "active",
	}
	s.nextID++
	s.tunnels[remotePort] = mapping
	s.listeners[remotePort] = ln

	s.addLog("[Tunnel] Created: :%d -> %s:%d (client=%s)", remotePort, localIP, localPort, clientName)

	// Accept connections
	go s.acceptConnections(ln, mapping)

	return nil
}

func (s *Server) removeTunnel(remotePort int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	mapping, exists := s.tunnels[remotePort]
	if !exists {
		return fmt.Errorf("port %d not found", remotePort)
	}

	if ln, ok := s.listeners[remotePort]; ok {
		ln.Close()
		delete(s.listeners, remotePort)
	}

	delete(s.tunnels, remotePort)
	s.addLog("[Tunnel] Removed: :%d (client=%s)", remotePort, mapping.ClientName)
	return nil
}

func (s *Server) acceptConnections(ln net.Listener, mapping *TunnelMapping) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}

		s.mu.RLock()
		client, ok := s.clients[mapping.ClientName]
		s.mu.RUnlock()

		if !ok {
			conn.Close()
			continue
		}

		// Generate work_id
		workID := fmt.Sprintf("%d_%d", mapping.RemotePort, time.Now().UnixNano())

		// Enqueue incoming connection
		s.mu.Lock()
		s.workQueue[workID] = conn
		s.mu.Unlock()

		// Notify client to establish work connection
		client.SendMsg(fmt.Sprintf("NEW_CONN %s %s %d", workID, mapping.LocalIP, mapping.LocalPort))

		// Timeout cleanup
		go func(wid string) {
			time.Sleep(15 * time.Second)
			s.mu.Lock()
			if c, ok := s.workQueue[wid]; ok {
				delete(s.workQueue, wid)
				c.Close()
			}
			s.mu.Unlock()
		}(workID)
	}
}

// ==================== Proxy Port ====================

func (s *Server) startProxyServer() {
	proxyPort := s.cfg.BindPort + 1
	addr := fmt.Sprintf("%s:%d", s.cfg.BindAddr, proxyPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("[Proxy] Listen failed: %v", err)
	}
	s.addLog("[Proxy] Listening on %s", addr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go s.handleProxyConn(conn)
	}
}

func (s *Server) handleProxyConn(conn net.Conn) {
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return
	}
	conn.SetDeadline(time.Time{})

	line = strings.TrimSpace(line)
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "WORK" {
		conn.Close()
		return
	}
	workID := fields[1]

	s.mu.Lock()
	incoming, ok := s.workQueue[workID]
	if ok {
		delete(s.workQueue, workID)
		s.mu.Unlock()
		go s.bridgeConnection(incoming, conn, workID)
	} else {
		s.workConns[workID] = conn
		s.mu.Unlock()
	}
}

// ==================== Web Dashboard ====================

func (s *Server) startWebServer() {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	// Static assets (embedded, no auth required)
	s.registerStaticRoutes(r)

	// Web auth middleware
	if s.cfg.WebUser != "" && s.cfg.WebPass != "" {
		authorized := r.Group("/", gin.BasicAuth(gin.Accounts{
			s.cfg.WebUser: s.cfg.WebPass,
		}))
		// Static pages
		authorized.GET("/", s.pageDashboard)
		authorized.GET("/tunnels", s.pageTunnels)
		authorized.GET("/clients", s.pageClients)
		// API
		authorized.GET("/api/stats", s.apiStats)
		authorized.GET("/api/tunnels", s.apiListTunnels)
		authorized.DELETE("/api/tunnels/:port", s.apiRemoveTunnel)
		authorized.GET("/api/clients", s.apiListClients)
		authorized.GET("/api/logs", s.apiLogs)
	} else {
		// Static pages
		r.GET("/", s.pageDashboard)
		r.GET("/tunnels", s.pageTunnels)
		r.GET("/clients", s.pageClients)
		// API
		r.GET("/api/stats", s.apiStats)
		r.GET("/api/tunnels", s.apiListTunnels)
		r.DELETE("/api/tunnels/:port", s.apiRemoveTunnel)
		r.GET("/api/clients", s.apiListClients)
		r.GET("/api/logs", s.apiLogs)
	}

	addr := fmt.Sprintf("%s:%d", s.cfg.BindAddr, s.cfg.WebPort)
	log.Printf("[Web] Dashboard on http://%s", addr)
	r.Run(addr)
}

func (s *Server) apiStats(c *gin.Context) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c.JSON(200, gin.H{
		"active_tunnels":    len(s.tunnels),
		"connected_clients": len(s.clients),
		"active_conns":      atomic.LoadInt64(&s.stats.ActiveConns),
		"total_connections": atomic.LoadInt64(&s.stats.TotalConnections),
		"total_traffic":     atomic.LoadInt64(&s.stats.TotalTraffic),
	})
}

func (s *Server) apiListTunnels(c *gin.Context) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]TunnelMapping, 0, len(s.tunnels))
	for _, t := range s.tunnels {
		list = append(list, *t)
	}
	c.JSON(200, list)
}

func (s *Server) apiRemoveTunnel(c *gin.Context) {
	port, _ := strconv.Atoi(c.Param("port"))
	if err := s.removeTunnel(port); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"message": "removed"})
}

func (s *Server) apiListClients(c *gin.Context) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type ClientInfo struct {
		Name          string    `json:"name"`
		RemoteIP      string    `json:"remote_ip"`
		Connected     time.Time `json:"connected"`
		LastHeartbeat time.Time `json:"last_heartbeat"`
		Uptime        int64     `json:"uptime_seconds"`
	}
	list := make([]ClientInfo, 0, len(s.clients))
	for _, cl := range s.clients {
		list = append(list, ClientInfo{
			Name:          cl.Name,
			RemoteIP:      cl.RemoteIP,
			Connected:     cl.Connected,
			LastHeartbeat: cl.LastHeartbeat,
			Uptime:        int64(time.Since(cl.Connected).Seconds()),
		})
	}
	c.JSON(200, list)
}

func (s *Server) apiLogs(c *gin.Context) {
	s.logMu.RLock()
	defer s.logMu.RUnlock()
	logs := make([]string, len(s.logs))
	copy(logs, s.logs)
	c.JSON(200, logs)
}

// ==================== Bootstrap Pages ====================

func (s *Server) registerStaticRoutes(r *gin.Engine) {
	r.GET("/static/bootstrap.min.css", func(c *gin.Context) {
		c.Header("Content-Type", "text/css; charset=utf-8")
		c.Header("Cache-Control", "public, max-age=31536000")
		c.Data(200, "text/css; charset=utf-8", bootstrapCSSData)
	})
	r.GET("/static/bootstrap.bundle.min.js", func(c *gin.Context) {
		c.Header("Content-Type", "application/javascript; charset=utf-8")
		c.Header("Cache-Control", "public, max-age=31536000")
		c.Data(200, "application/javascript; charset=utf-8", bootstrapJSData)
	})
}

const pageStyle = `<style>
:root{--tx-bg:#0a0e1a;--tx-card:#111827;--tx-border:#1e293b;--tx-text:#e2e8f0;--tx-muted:#64748b;--tx-accent:#38bdf8;--tx-green:#34d399;--tx-amber:#fbbf24;--tx-rose:#fb7185}
*{box-sizing:border-box}
body{background:var(--tx-bg);color:var(--tx-text);font-family:'Segoe UI',system-ui,-apple-system,sans-serif;margin:0;min-height:100vh}
.navbar{background:rgba(17,24,39,.85);backdrop-filter:blur(12px);border-bottom:1px solid var(--tx-border);padding:.75rem 0;position:sticky;top:0;z-index:100}
.navbar-brand{font-weight:700;font-size:1.25rem;color:var(--tx-accent)!important;letter-spacing:-.02em}
.navbar-brand span{color:var(--tx-text);font-weight:400;opacity:.6;font-size:.75rem;margin-left:.5rem}
.nav-link{color:var(--tx-muted)!important;font-weight:500;padding:.5rem 1rem!important;border-radius:.5rem;transition:all .2s}
.nav-link:hover{color:var(--tx-text)!important;background:rgba(56,189,248,.08)}
.nav-link.active{color:var(--tx-accent)!important;background:rgba(56,189,248,.12)}
.stat-card{background:var(--tx-card);border:1px solid var(--tx-border);border-radius:.75rem;padding:1.25rem;text-align:center;transition:all .3s}
.stat-card:hover{border-color:var(--tx-accent);box-shadow:0 0 20px rgba(56,189,248,.1);transform:translateY(-2px)}
.stat-label{color:var(--tx-muted);font-size:.8rem;font-weight:500;text-transform:uppercase;letter-spacing:.05em;margin-bottom:.5rem}
.stat-value{font-size:1.75rem;font-weight:700;line-height:1}
.panel{background:var(--tx-card);border:1px solid var(--tx-border);border-radius:.75rem;padding:1.25rem;margin-bottom:1rem}
.panel-title{color:var(--tx-text);font-weight:600;font-size:1rem;margin-bottom:1rem;padding-bottom:.75rem;border-bottom:1px solid var(--tx-border)}
.table{color:var(--tx-text);margin:0}
.table>:not(caption)>*>*{border-color:var(--tx-border);padding:.6rem .5rem;background:transparent}
.table thead th{color:var(--tx-muted);font-weight:600;font-size:.8rem;text-transform:uppercase;letter-spacing:.05em;border-bottom-width:1px}
.table tbody tr{background:#111827}
.table tbody tr:hover{background:#1a2332}
.badge-active{background:rgba(52,211,153,.15);color:var(--tx-green);padding:.35rem .6rem;border-radius:.375rem;font-weight:600;font-size:.75rem}
.badge-inactive{background:rgba(251,113,133,.15);color:var(--tx-rose);padding:.35rem .6rem;border-radius:.375rem;font-weight:600;font-size:.75rem}
.btn-del{background:rgba(251,113,133,.1);color:var(--tx-rose);border:1px solid rgba(251,113,133,.2);padding:.25rem .6rem;border-radius:.375rem;font-size:.75rem;cursor:pointer;transition:all .2s}
.btn-del:hover{background:rgba(251,113,133,.2);border-color:var(--tx-rose)}
.log-box{background:#080c14;border:1px solid var(--tx-border);border-radius:.5rem;padding:.75rem 1rem;font-family:'Cascadia Code',Consolas,monospace;font-size:.8rem;line-height:1.6;color:var(--tx-muted);max-height:400px;overflow-y:auto;white-space:pre-wrap;word-break:break-all}
.log-box::-webkit-scrollbar{width:6px}
.log-box::-webkit-scrollbar-track{background:transparent}
.log-box::-webkit-scrollbar-thumb{background:var(--tx-border);border-radius:3px}
.empty-state{text-align:center;padding:2rem;color:var(--tx-muted)}
.empty-state .icon{font-size:2rem;margin-bottom:.5rem;opacity:.5}
</style>`

const pageNav = `<nav class="navbar navbar-expand-lg"><div class="container">
<a class="navbar-brand" href="/">TianXing Tunnel<span>v1.0</span></a>
<div class="navbar-nav ms-auto">
<a class="nav-link NAV_DASH" href="/">仪表盘</a>
<a class="nav-link NAV_TUNNEL" href="/tunnels">隧道</a>
<a class="nav-link NAV_CLIENT" href="/clients">客户端</a>
</div></div></nav>`

func (s *Server) pageDashboard(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(200, `<!DOCTYPE html>
<html lang="zh-CN" data-bs-theme="dark">
<head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>TianXing Tunnel - 仪表盘</title>
<link href="/static/bootstrap.min.css" rel="stylesheet">
`+pageStyle+`
</head>
<body>
`+strings.Replace(pageNav, "NAV_DASH", "active", 1)+`
<div class="container mt-4">
<div class="row g-3 mb-4">
<div class="col-md-3"><div class="stat-card"><div class="stat-label">活跃隧道</div><div class="stat-value" style="color:var(--tx-accent)" id="tunnelCount">-</div></div></div>
<div class="col-md-3"><div class="stat-card"><div class="stat-label">在线客户端</div><div class="stat-value" style="color:var(--tx-green)" id="clientCount">-</div></div></div>
<div class="col-md-3"><div class="stat-card"><div class="stat-label">活跃连接</div><div class="stat-value" style="color:var(--tx-amber)" id="activeConns">-</div></div></div>
<div class="col-md-3"><div class="stat-card"><div class="stat-label">总流量</div><div class="stat-value" style="color:var(--tx-rose)" id="totalTraffic">-</div></div></div>
</div>
<div class="panel"><div class="panel-title">系统日志</div><div class="log-box" id="logs"></div></div>
</div>
<script src="/static/bootstrap.bundle.min.js"></script>
<script>
function updateStats(){
fetch('/api/stats').then(r=>r.json()).then(d=>{
document.getElementById('tunnelCount').textContent=d.active_tunnels;
document.getElementById('clientCount').textContent=d.connected_clients;
document.getElementById('activeConns').textContent=d.active_conns;
document.getElementById('totalTraffic').textContent=(d.total_traffic/1024/1024).toFixed(2)+' MB';
}).catch(e=>console.error(e));
}
function updateLogs(){
fetch('/api/logs').then(r=>r.json()).then(d=>{
var el=document.getElementById('logs');
el.textContent=d.join('\n');
el.scrollTop=el.scrollHeight;
}).catch(e=>console.error(e));
}
updateStats();updateLogs();
setInterval(updateStats,2000);
setInterval(updateLogs,3000);
</script>
</body></html>`)
}

func (s *Server) pageTunnels(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(200, `<!DOCTYPE html>
<html lang="zh-CN" data-bs-theme="dark">
<head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>TianXing Tunnel - 隧道管理</title>
<link href="/static/bootstrap.min.css" rel="stylesheet">
`+pageStyle+`
</head>
<body>
`+strings.Replace(pageNav, "NAV_TUNNEL", "active", 1)+`
<div class="container mt-4">
<div class="panel"><div class="panel-title">隧道列表</div>
<table class="table table-sm"><thead><tr><th>远程端口</th><th>类型</th><th>本地地址</th><th>客户端</th><th>状态</th><th>操作</th></tr></thead>
<tbody id="tunnelList"></tbody></table>
<div class="empty-state" id="emptyTunnel" style="display:none"><div class="icon">&#9881;</div><div>暂无活跃隧道</div></div>
</div></div>
<script src="/static/bootstrap.bundle.min.js"></script>
<script>
function load(){
fetch('/api/tunnels').then(r=>r.json()).then(d=>{
var tb=document.getElementById('tunnelList');tb.innerHTML='';
var empty=document.getElementById('emptyTunnel');
if(d.length===0){empty.style.display='block';return;}
empty.style.display='none';
d.forEach(t=>{
var tr=document.createElement('tr');
var badge=t.status==='active'?'badge-active':'badge-inactive';
tr.innerHTML='<td><strong>:'+t.remote_port+'</strong></td><td>'+t.type.toUpperCase()+'</td><td>'+t.local_ip+':'+t.local_port+'</td><td>'+t.client_name+'</td><td><span class="'+badge+'">'+t.status+'</span></td><td><button class="btn-del" onclick="removeTunnel('+t.remote_port+')">删除</button></td>';
tb.appendChild(tr);
});
}).catch(e=>console.error(e));
}
function removeTunnel(port){if(!confirm('确认删除端口 '+port+' 的隧道?'))return;fetch('/api/tunnels/'+port,{method:'DELETE'}).then(()=>load());}
load();setInterval(load,5000);
</script>
</body></html>`)
}

func (s *Server) pageClients(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(200, `<!DOCTYPE html>
<html lang="zh-CN" data-bs-theme="dark">
<head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>TianXing Tunnel - 客户端</title>
<link href="/static/bootstrap.min.css" rel="stylesheet">
`+pageStyle+`
</head>
<body>
`+strings.Replace(pageNav, "NAV_CLIENT", "active", 1)+`
<div class="container mt-4">
<div class="panel"><div class="panel-title">在线客户端</div>
<table class="table table-sm"><thead><tr><th>名称</th><th>IP 地址</th><th>连接时间</th><th>在线时长</th><th>最后心跳</th></tr></thead>
<tbody id="clientList"></tbody></table>
<div class="empty-state" id="emptyClient" style="display:none"><div class="icon">&#128274;</div><div>暂无在线客户端</div></div>
</div></div>
<script src="/static/bootstrap.bundle.min.js"></script>
<script>
function fmtDur(sec){
var h=Math.floor(sec/3600),m=Math.floor(sec%3600/60),s=sec%60;
if(h>0) return h+'h '+m+'m '+s+'s';
if(m>0) return m+'m '+s+'s';
return s+'s';
}
function fmtTimeAgo(t){
var sec=Math.floor((Date.now()-new Date(t).getTime())/1000);
if(sec<5) return '<span style="color:var(--tx-green)">刚刚</span>';
if(sec<60) return sec+'秒前';
if(sec<300) return '<span style="color:var(--tx-amber)">'+Math.floor(sec/60)+'分前</span>';
return '<span style="color:var(--tx-rose)">'+Math.floor(sec/60)+'分前</span>';
}
function load(){
fetch('/api/clients').then(r=>r.json()).then(d=>{
var tb=document.getElementById('clientList');tb.innerHTML='';
var empty=document.getElementById('emptyClient');
if(d.length===0){empty.style.display='block';return;}
empty.style.display='none';
d.forEach(c=>{
var tr=document.createElement('tr');
tr.innerHTML='<td><strong>'+c.name+'</strong></td><td><code>'+c.remote_ip+'</code></td><td>'+new Date(c.connected).toLocaleString()+'</td><td>'+fmtDur(c.uptime_seconds)+'</td><td>'+fmtTimeAgo(c.last_heartbeat)+'</td>';
tb.appendChild(tr);
});
}).catch(e=>console.error(e));
}
load();setInterval(load,3000);
</script>
</body></html>`)
}

// ==================== Utility Functions ====================

func parsePortRange(s string) (int, int, error) {
	if !strings.Contains(s, "-") {
		p, err := strconv.Atoi(s)
		return p, p, err
	}
	parts := strings.SplitN(s, "-", 2)
	start, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	end, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, err
	}
	if start > end {
		return 0, 0, fmt.Errorf("invalid port range: start > end")
	}
	return start, end, nil
}

// ==================== Main Entry ====================

const banner = `
___________.__              ____  ___.__                
\__    ___/|__|____    ____ \   \/  /|__| ____    ____  
  |    |   |  \__  \  /    \ \     / |  |/    \  / ___\ 
  |    |   |  |/ __ \|   |  \/     \ |  |   |  \/ /_/  >
  |____|   |__(____  /___|  /___/\  \|__|___|  /\___  / 
                   \/     \/      \_/        \//_____/  
   Tunnel Server v%s
`

func main() {
	// Setup logging to file
	setupLogging()

	if len(os.Args) > 1 && (os.Args[1] == "-v" || os.Args[1] == "--version") {
		fmt.Printf("TianXing Tunnel Server v%s (build: %s, go: %s)\n", Version, BuildTime, GoVersion)
		return
	}

	if isTerminal() {
		fmt.Printf(banner, Version)
		fmt.Printf("  Build: %s | Go: %s\n\n", BuildTime, GoVersion)
	} else {
		log.Printf("[TianXing] Server v%s (build: %s, go: %s)", Version, BuildTime, GoVersion)
	}

	configPath := "tianxing.conf"
	if len(os.Args) > 1 && os.Args[1] != "-v" && os.Args[1] != "--version" {
		configPath = os.Args[1]
	}

	cfg := loadServerConfig(configPath)
	log.Printf("[TianXing] Server starting...")
	log.Printf("[TianXing] Control port: %d, Web port: %d", cfg.BindPort, cfg.WebPort)

	s := NewServer(cfg)

	go s.startControlServer()
	go s.startProxyServer()

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("[TianXing] Received signal %v, shutting down gracefully...", sig)

		s.mu.Lock()
		for port, ln := range s.listeners {
			ln.Close()
			delete(s.listeners, port)
		}
		for name, cl := range s.clients {
			cl.Conn.Close()
			delete(s.clients, name)
		}
		s.mu.Unlock()

		log.Printf("[TianXing] Server stopped")
		os.Exit(0)
	}()

	s.startWebServer()
}
