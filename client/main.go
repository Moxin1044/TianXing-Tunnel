package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
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

type ClientConfig struct {
	ServerAddr string `json:"server_addr"`
	ServerPort int    `json:"server_port"`
	Token      string `json:"token"`
	Name       string `json:"name"`
	APIPort    int    `json:"api_port"`
	APIToken   string `json:"api_token"`
}

type MappingRule struct {
	Type       string `json:"type"`        // tcp, udp
	RemotePort int    `json:"remote_port"` // Server listen port
	LocalIP    string `json:"local_ip"`    // Local service IP
	LocalPort  int    `json:"local_port"`  // Local service port
}

func loadClientConfig(path string) *ClientConfig {
	cfg := &ClientConfig{
		ServerAddr: "127.0.0.1",
		ServerPort: 7000,
		Token:      "",
		Name:       "default",
		APIPort:    7500,
		APIToken:   "",
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
		// Skip [mapping] section marker
		if strings.HasPrefix(line, "[") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		switch key {
		case "server_addr":
			cfg.ServerAddr = val
		case "server_port":
			cfg.ServerPort, _ = strconv.Atoi(val)
		case "token":
			cfg.Token = val
		case "name":
			cfg.Name = val
		case "api_port":
			cfg.APIPort, _ = strconv.Atoi(val)
		case "api_token":
			cfg.APIToken = val
		}
	}
	return cfg
}

func loadMappings(path string) []MappingRule {
	var rules []MappingRule
	data, err := os.ReadFile(path)
	if err != nil {
		return rules
	}
	lines := strings.Split(string(data), "\n")
	inMapping := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "[mapping]" {
			inMapping = true
			continue
		}
		if strings.HasPrefix(line, "[") {
			inMapping = false
			continue
		}
		if !inMapping || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Format: type=tcp remote_port=8080 local_ip=127.0.0.1 local_port=80
		// Or: tcp 8080 127.0.0.1 80
		rule := parseMappingLine(line)
		if rule != nil {
			rules = append(rules, *rule)
		}
	}
	return rules
}

func parseMappingLine(line string) *MappingRule {
	// Format1: type=tcp remote_port=8080 local_ip=127.0.0.1 local_port=80
	if strings.Contains(line, "=") {
		rule := &MappingRule{}
		for _, kv := range strings.Fields(line) {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				continue
			}
			switch parts[0] {
			case "type":
				rule.Type = parts[1]
			case "remote_port":
				rule.RemotePort, _ = strconv.Atoi(parts[1])
			case "local_ip":
				rule.LocalIP = parts[1]
			case "local_port":
				rule.LocalPort, _ = strconv.Atoi(parts[1])
			}
		}
		if rule.Type == "" {
			rule.Type = "tcp"
		}
		if rule.RemotePort > 0 && rule.LocalPort > 0 {
			return rule
		}
		return nil
	}
	// Format2: tcp 8080 127.0.0.1 80
	fields := strings.Fields(line)
	if len(fields) < 4 {
		return nil
	}
	remotePort, _ := strconv.Atoi(fields[1])
	localPort, _ := strconv.Atoi(fields[3])
	if remotePort > 0 && localPort > 0 {
		return &MappingRule{
			Type:       fields[0],
			RemotePort: remotePort,
			LocalIP:    fields[2],
			LocalPort:  localPort,
		}
	}
	return nil
}

// ==================== Client Core ====================

type Client struct {
	cfg       *ClientConfig
	conn      net.Conn
	reader    *bufio.Reader
	writer    *bufio.Writer
	mappings  map[int]*MappingRule // remote_port -> rule
	mu        sync.RWMutex
	readMu    sync.Mutex // Protect read
	writeMu   sync.Mutex // Protect write
	connected bool
	stopCh    chan struct{}
	stats     ClientStats
}

type ClientStats struct {
	ActiveConns  int64 `json:"active_conns"`
	TotalConns   int64 `json:"total_conns"`
	TotalTraffic int64 `json:"total_traffic"`
}

func NewClient(cfg *ClientConfig) *Client {
	return &Client{
		cfg:      cfg,
		mappings: make(map[int]*MappingRule),
	}
}

func (c *Client) connect() error {
	addr := fmt.Sprintf("%s:%d", c.cfg.ServerAddr, c.cfg.ServerPort)
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("connect to server failed: %v", err)
	}

	c.conn = conn
	c.reader = bufio.NewReader(conn)
	c.writer = bufio.NewWriter(conn)

	// AUTH
	c.sendMsg(fmt.Sprintf("AUTH %s %s", c.cfg.Token, c.cfg.Name))
	resp, err := c.readMsg()
	if err != nil {
		conn.Close()
		return fmt.Errorf("auth failed: %v", err)
	}
	if resp != "OK" {
		conn.Close()
		return fmt.Errorf("auth failed: %s", resp)
	}

	c.connected = true
	log.Printf("[Client] Connected to %s, name=%s", addr, c.cfg.Name)
	return nil
}

func (c *Client) sendMsg(msg string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	c.writer.WriteString(msg + "\n")
	err := c.writer.Flush()
	c.conn.SetWriteDeadline(time.Time{})
	return err
}

func (c *Client) readMsg() (string, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	c.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	line, err := c.reader.ReadString('\n')
	c.conn.SetReadDeadline(time.Time{})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (c *Client) run() {
	for {
		if err := c.connect(); err != nil {
			log.Printf("[Client] Connect error: %v, retrying in 5s...", err)
			time.Sleep(5 * time.Second)
			continue
		}

		// Re-register all mappings
		c.reregisterAll()

		// Heartbeat + command loop
		go c.heartbeat()
		c.commandLoop()

		c.connected = false
		log.Printf("[Client] Disconnected, reconnecting...")
		time.Sleep(3 * time.Second)
	}
}

func (c *Client) reregisterAll() {
	c.mu.RLock()
	rules := make([]*MappingRule, 0, len(c.mappings))
	for _, r := range c.mappings {
		rules = append(rules, r)
	}
	c.mu.RUnlock()

	for _, r := range rules {
		err := c.addMappingToServer(r)
		if err != nil {
			log.Printf("[Client] Re-register port %d failed: %v", r.RemotePort, err)
		} else {
			log.Printf("[Client] Re-registered port %d", r.RemotePort)
		}
	}
}

func (c *Client) heartbeat() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if !c.connected {
			return
		}
		if err := c.sendMsg("PING"); err != nil {
			return
		}
	}
}

func (c *Client) commandLoop() {
	for {
		msg, err := c.readMsg()
		if err != nil {
			return
		}

		fields := strings.Fields(msg)
		if len(fields) == 0 {
			continue
		}

		switch fields[0] {
		case "PONG":
			// Heartbeat reply
		case "NEW_CONN":
			// NEW_CONN <work_id> <local_ip> <local_port>
			if len(fields) < 4 {
				continue
			}
			workID := fields[1]
			localIP := fields[2]
			localPort, _ := strconv.Atoi(fields[3])
			go c.handleNewConn(workID, localIP, localPort)
		case "ADD_OK":
			log.Printf("[Client] Tunnel added: port %s", strings.Join(fields[1:], " "))
		case "ADD_FAILED":
			log.Printf("[Client] Add failed: %s", strings.Join(fields[1:], " "))
		case "ADD_RANGE_OK":
			log.Printf("[Client] Range tunnel added: %s", strings.Join(fields[1:], " "))
		}
	}
}

func (c *Client) handleNewConn(workID, localIP string, localPort int) {
	// Connect to local service
	localAddr := fmt.Sprintf("%s:%d", localIP, localPort)
	localConn, err := net.DialTimeout("tcp", localAddr, 5*time.Second)
	if err != nil {
		log.Printf("[Client] Connect to local %s failed: %v", localAddr, err)
		return
	}

	// Connect to server proxy port
	proxyPort := c.cfg.ServerPort + 1
	proxyAddr := fmt.Sprintf("%s:%d", c.cfg.ServerAddr, proxyPort)
	proxyConn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		localConn.Close()
		log.Printf("[Client] Connect to proxy failed: %v", err)
		return
	}

	// Send WORK work_id
	proxyConn.Write([]byte(fmt.Sprintf("WORK %s\n", workID)))

	atomic.AddInt64(&c.stats.ActiveConns, 1)
	atomic.AddInt64(&c.stats.TotalConns, 1)

	var traffic int64
	done := make(chan struct{}, 2)

	// Use 64KB buffer for high-throughput data transfer
	buf := make([]byte, 64*1024)

	cp := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		n, _ := io.CopyBuffer(dst, src, buf)
		atomic.AddInt64(&traffic, n)
		dst.Close()
	}

	go cp(localConn, proxyConn)
	go cp(proxyConn, localConn)

	<-done
	atomic.AddInt64(&c.stats.ActiveConns, -1)
	atomic.AddInt64(&c.stats.TotalTraffic, traffic)
}

// ==================== Mapping Management ====================

func (c *Client) addMappingToServer(rule *MappingRule) error {
	return c.sendMsg(fmt.Sprintf("ADD %s %d %s %d", rule.Type, rule.RemotePort, rule.LocalIP, rule.LocalPort))
}

func (c *Client) AddMapping(rule *MappingRule) error {
	c.mu.Lock()
	c.mappings[rule.RemotePort] = rule
	c.mu.Unlock()

	if c.connected {
		return c.addMappingToServer(rule)
	}
	return nil
}

func (c *Client) AddRangeMapping(tunnelType string, portStart, portEnd int, localIP string) error {
	// Add to local mappings first
	for port := portStart; port <= portEnd; port++ {
		c.mu.Lock()
		c.mappings[port] = &MappingRule{
			Type:       tunnelType,
			RemotePort: port,
			LocalIP:    localIP,
			LocalPort:  port,
		}
		c.mu.Unlock()
	}

	if !c.connected {
		return fmt.Errorf("will register after reconnection")
	}
	return c.sendMsg(fmt.Sprintf("ADD_RANGE %s %d-%d %s", tunnelType, portStart, portEnd, localIP))
}

func (c *Client) RemoveMapping(remotePort int) error {
	c.mu.Lock()
	delete(c.mappings, remotePort)
	c.mu.Unlock()

	if c.connected {
		return c.sendMsg(fmt.Sprintf("REMOVE %d", remotePort))
	}
	return nil
}

func (c *Client) ListMappings() []MappingRule {
	c.mu.RLock()
	defer c.mu.RUnlock()
	list := make([]MappingRule, 0, len(c.mappings))
	for _, r := range c.mappings {
		list = append(list, *r)
	}
	return list
}

// ==================== Local API ====================

func (c *Client) startAPI() {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	api := r.Group("/api")
	api.Use(c.apiAuth())
	{
		api.GET("/mappings", c.apiListMappings)
		api.POST("/mappings", c.apiAddMapping)
		api.POST("/mappings/range", c.apiAddRangeMapping)
		api.DELETE("/mappings/:port", c.apiRemoveMapping)
		api.GET("/stats", c.apiStats)
		api.GET("/status", c.apiStatus)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", c.cfg.APIPort)
	log.Printf("[API] Listening on http://%s", addr)
	r.Run(addr)
}

func (c *Client) apiAuth() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		if c.cfg.APIToken == "" {
			ctx.Next()
			return
		}
		auth := ctx.GetHeader("Authorization")
		if auth == "" {
			ctx.JSON(http.StatusUnauthorized, gin.H{"error": "missing authorization header"})
			ctx.Abort()
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		if token != c.cfg.APIToken {
			ctx.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			ctx.Abort()
			return
		}
		ctx.Next()
	}
}

func (c *Client) apiListMappings(ctx *gin.Context) {
	ctx.JSON(200, c.ListMappings())
}

func (c *Client) apiAddMapping(ctx *gin.Context) {
	var rule MappingRule
	if err := ctx.ShouldBindJSON(&rule); err != nil {
		ctx.JSON(400, gin.H{"error": "invalid request"})
		return
	}
	if rule.Type == "" {
		rule.Type = "tcp"
	}
	if rule.LocalIP == "" {
		rule.LocalIP = "127.0.0.1"
	}
	if err := c.AddMapping(&rule); err != nil {
		ctx.JSON(500, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(200, rule)
}

func (c *Client) apiAddRangeMapping(ctx *gin.Context) {
	var req struct {
		Type      string `json:"type"`
		PortStart int    `json:"port_start"`
		PortEnd   int    `json:"port_end"`
		LocalIP   string `json:"local_ip"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(400, gin.H{"error": "invalid request"})
		return
	}
	if req.Type == "" {
		req.Type = "tcp"
	}
	if req.LocalIP == "" {
		req.LocalIP = "127.0.0.1"
	}
	if err := c.AddRangeMapping(req.Type, req.PortStart, req.PortEnd, req.LocalIP); err != nil {
		// Non-fatal error, mapping added locally, will register after reconnection
		ctx.JSON(200, gin.H{"message": "range mapping added (will register after reconnection)", "port_start": req.PortStart, "port_end": req.PortEnd, "warning": err.Error()})
		return
	}
	ctx.JSON(200, gin.H{"message": "range mapping added", "port_start": req.PortStart, "port_end": req.PortEnd})
}

func (c *Client) apiRemoveMapping(ctx *gin.Context) {
	port, _ := strconv.Atoi(ctx.Param("port"))
	if err := c.RemoveMapping(port); err != nil {
		ctx.JSON(500, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(200, gin.H{"message": "removed"})
}

func (c *Client) apiStats(ctx *gin.Context) {
	ctx.JSON(200, gin.H{
		"active_conns":  atomic.LoadInt64(&c.stats.ActiveConns),
		"total_conns":   atomic.LoadInt64(&c.stats.TotalConns),
		"total_traffic": atomic.LoadInt64(&c.stats.TotalTraffic),
	})
}

func (c *Client) apiStatus(ctx *gin.Context) {
	ctx.JSON(200, gin.H{
		"connected":     c.connected,
		"server":        fmt.Sprintf("%s:%d", c.cfg.ServerAddr, c.cfg.ServerPort),
		"name":          c.cfg.Name,
		"mapping_count": len(c.mappings),
	})
}

// ==================== CLI Commands ====================

func printUsage() {
	fmt.Printf(`TianXing Tunnel Client v%s

Usage:
  tianxing-client [config file path]                           Start the client
  tianxing-client add <remote_port> <local_ip> <local_port>   Add a mapping
  tianxing-client range <start_port>-<end_port> <local_ip>    Add a port range mapping
  tianxing-client remove <remote_port>                         Remove a mapping
  tianxing-client list                                         List all mappings
  tianxing-client version                                      Show version
  tianxing-client help                                         Show help

Config file default: tianxing.conf`, Version)
}

func runCLICommand(cfg *ClientConfig, args []string) {
	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}

	apiAddr := fmt.Sprintf("http://127.0.0.1:%d", cfg.APIPort)
	client := &http.Client{Timeout: 5 * time.Second}

	switch args[0] {
	case "add":
		if len(args) < 4 {
			fmt.Println("Usage: tianxing-client add <remote_port> <local_ip> <local_port>")
			os.Exit(1)
		}
		remotePort, _ := strconv.Atoi(args[1])
		localPort, _ := strconv.Atoi(args[3])
		body, _ := json.Marshal(MappingRule{Type: "tcp", RemotePort: remotePort, LocalIP: args[2], LocalPort: localPort})
		req, _ := http.NewRequest("POST", apiAddr+"/api/mappings", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		if cfg.APIToken != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
		}
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		var result map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&result)
		if resp.StatusCode == 200 {
			fmt.Printf("Mapping added: :%d -> %s:%d\n", remotePort, args[2], localPort)
		} else {
			fmt.Printf("Add failed: %v\n", result["error"])
		}

	case "range":
		if len(args) < 3 {
			fmt.Println("Usage: tianxing-client range <start_port>-<end_port> <local_ip>")
			os.Exit(1)
		}
		portParts := strings.SplitN(args[1], "-", 2)
		start, _ := strconv.Atoi(portParts[0])
		end, _ := strconv.Atoi(portParts[1])
		body, _ := json.Marshal(map[string]interface{}{"type": "tcp", "port_start": start, "port_end": end, "local_ip": args[2]})
		req, _ := http.NewRequest("POST", apiAddr+"/api/mappings/range", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		if cfg.APIToken != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
		}
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			fmt.Printf("Port range mapping added: %d-%d -> %s\n", start, end, args[2])
		} else {
			var result map[string]interface{}
			json.NewDecoder(resp.Body).Decode(&result)
			fmt.Printf("Add failed: %v\n", result["error"])
		}

	case "remove":
		if len(args) < 2 {
			fmt.Println("Usage: tianxing-client remove <remote_port>")
			os.Exit(1)
		}
		req, _ := http.NewRequest("DELETE", apiAddr+"/api/mappings/"+args[1], nil)
		if cfg.APIToken != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
		}
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			fmt.Printf("Mapping removed: port %s\n", args[1])
		} else {
			fmt.Println("Remove failed")
		}

	case "list":
		req, _ := http.NewRequest("GET", apiAddr+"/api/mappings", nil)
		if cfg.APIToken != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
		}
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		var mappings []MappingRule
		json.NewDecoder(resp.Body).Decode(&mappings)
		if len(mappings) == 0 {
			fmt.Println("No mappings")
			return
		}
		fmt.Printf("%-12s %-8s %-20s %-10s\n", "Remote Port", "Type", "Local Address", "Status")
		fmt.Println(strings.Repeat("-", 54))
		for _, m := range mappings {
			fmt.Printf("%-12d %-8s %-20s %-10s\n", m.RemotePort, m.Type, m.LocalIP+":"+strconv.Itoa(m.LocalPort), "active")
		}

	case "version", "-v", "--version":
		fmt.Printf("TianXing Tunnel Client v%s (build: %s, go: %s)\n", Version, BuildTime, GoVersion)

	case "help", "--help", "-h":
		printUsage()

	default:
		fmt.Printf("Unknown command: %s\n", args[0])
		printUsage()
		os.Exit(1)
	}
}

// ==================== Main Entry ====================

const clientBanner = `
  _____ _   _  ___   __ _____ _____
 |_   _| | | | \ \ / /|_   _|  __ \
   | | | | | |  \ V /   | | | |  | |
   | | | |_| |   | |    | | | |  | |
   |_|  \___/    |_|    |_| |_|  |_|
   Tunnel Client v%s
`

func main() {
	// Setup logging to file
	setupLogging()

	// Parse arguments
	args := os.Args[1:]
	cliCommands := map[string]bool{"add": true, "remove": true, "list": true, "range": true, "help": true, "--help": true, "-h": true, "version": true, "-v": true, "--version": true}

	// Version command
	if len(args) == 1 && (args[0] == "-v" || args[0] == "--version" || args[0] == "version") {
		fmt.Printf("TianXing Tunnel Client v%s (build: %s, go: %s)\n", Version, BuildTime, GoVersion)
		return
	}

	configPath := "tianxing.conf"
	if len(args) > 0 && !cliCommands[args[0]] {
		configPath = args[0]
		args = args[1:]
	}

	cfg := loadClientConfig(configPath)

	// CLI command mode
	if len(args) > 0 && cliCommands[args[0]] {
		runCLICommand(cfg, args)
		return
	}

	// Normal startup mode
	if isTerminal() {
		fmt.Printf(clientBanner, Version)
		fmt.Printf("  Build: %s | Go: %s\n\n", BuildTime, GoVersion)
	} else {
		log.Printf("[TianXing] Client v%s (build: %s, go: %s)", Version, BuildTime, GoVersion)
	}

	if cfg.Token == "" {
		log.Fatal("[Client] Token is required, set 'token' in config file")
	}

	client := NewClient(cfg)

	// Load mappings from config file
	rules := loadMappings(configPath)
	for _, rule := range rules {
		client.mu.Lock()
		client.mappings[rule.RemotePort] = &rule
		client.mu.Unlock()
		log.Printf("[Client] Loaded mapping: :%d -> %s:%d", rule.RemotePort, rule.LocalIP, rule.LocalPort)
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("[Client] Received signal %v, shutting down...", sig)
		if client.conn != nil {
			client.conn.Close()
		}
		os.Exit(0)
	}()

	// Start API server
	go client.startAPI()

	// Start connection loop
	client.run()
}
