package main

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/skip2/go-qrcode"
)

const defaultPairTTL = 10 * time.Minute

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(_ *http.Request) bool {
		return true
	},
}

var sessionCounter uint64

type appConfig struct {
	Addr               string
	DefaultCWD         string
	LegacyToken        string
	ClaudeBin          string
	CodexBin           string
	SkipAuth           bool
	StatePath          string
	QRPath             string
	PairCodeTTL        time.Duration
	PublicHost         string
	PublicPort         string
	WSScheme           string
	EnableQRLogs       bool
	EnforceAccessToken bool
	AccessTokenTTL     time.Duration
	AllowedOrigins     map[string]struct{}
	TunnelAutostart    bool
	TunnelBin          string
	TunnelTargetURL    string
}

type inboundMessage struct {
	Type        string   `json:"type"`
	Token       string   `json:"token,omitempty"`
	AccessToken string   `json:"access_token,omitempty"`
	SessionID   string   `json:"session_id,omitempty"`
	Tool        string   `json:"tool,omitempty"`
	Args        []string `json:"args,omitempty"`
	CWD         string   `json:"cwd,omitempty"`
	Data        string   `json:"data,omitempty"`
	Cols        uint16   `json:"cols,omitempty"`
	Rows        uint16   `json:"rows,omitempty"`
	PairCode    string   `json:"pair_code,omitempty"`
	DeviceID    string   `json:"device_id,omitempty"`
	DeviceName  string   `json:"device_name,omitempty"`
}

type outboundMessage struct {
	Type         string   `json:"type"`
	SessionID    string   `json:"session_id,omitempty"`
	Message      string   `json:"message,omitempty"`
	Data         string   `json:"data,omitempty"`
	Stream       string   `json:"stream,omitempty"`
	Tool         string   `json:"tool,omitempty"`
	CWD          string   `json:"cwd,omitempty"`
	ExitCode     *int     `json:"exit_code,omitempty"`
	Sessions     []string `json:"sessions,omitempty"`
	DeviceID     string   `json:"device_id,omitempty"`
	DeviceToken  string   `json:"device_token,omitempty"`
	AgentID      string   `json:"agent_id,omitempty"`
	PairCode     string   `json:"pair_code,omitempty"`
	PairExpires  string   `json:"pair_expires_at,omitempty"`
	PairingWSURL string   `json:"pairing_ws_url,omitempty"`
	AccessToken  string   `json:"access_token,omitempty"`
	AccessExpire string   `json:"access_expires_at,omitempty"`
	At           string   `json:"at"`
}

type session struct {
	id       string
	tool     string
	cwd      string
	model    string
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   io.ReadCloser
	mu       sync.RWMutex
	claudeID string
	running  bool
	runCmd   *exec.Cmd
	stopped  chan struct{}
	stopOnce sync.Once
}

type pairedDevice struct {
	DeviceName string `json:"device_name"`
	Token      string `json:"token"`
	PairedAt   string `json:"paired_at"`
	LastSeenAt string `json:"last_seen_at"`
}

type persistentState struct {
	AgentID       string                  `json:"agent_id"`
	PairedDevices map[string]pairedDevice `json:"paired_devices"`
}

type stateStore struct {
	path string
	mu   sync.Mutex
	data persistentState
}

type pairingSnapshot struct {
	Code      string
	ExpiresAt time.Time
	Payload   string
	WSURL     string
}

type remoteDirItem struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type listDirsResponse struct {
	Path string          `json:"path"`
	Dirs []remoteDirItem `json:"dirs"`
}

type agentServer struct {
	cfg   appConfig
	store *stateStore

	pairMu      sync.RWMutex
	pairCode    string
	pairExpires time.Time
	tunnel      *tunnelManager
}

type connectionState struct {
	server       *agentServer
	conn         *websocket.Conn
	writeMu      sync.Mutex
	sessions     map[string]*session
	sessionMu    sync.RWMutex
	authed       bool
	deviceID     string
	accessToken  string
	accessExpire time.Time
	logPrefix    string
}

type tunnelManager struct {
	bin       string
	targetURL string

	mu        sync.RWMutex
	cmd       *exec.Cmd
	running   bool
	publicURL string
	wsURL     string
	startedAt time.Time
	lastError string
}

type tunnelStatus struct {
	Enabled   bool   `json:"enabled"`
	Ready     bool   `json:"ready"`
	PublicURL string `json:"public_url,omitempty"`
	WSURL     string `json:"ws_url,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	store, err := newStateStore(cfg.StatePath)
	if err != nil {
		log.Fatalf("state init error: %v", err)
	}

	tunnel := newTunnelManager(cfg.TunnelBin, cfg.TunnelTargetURL)
	agent := &agentServer{cfg: cfg, store: store, tunnel: tunnel}
	if _, err := agent.refreshPairing("startup"); err != nil {
		log.Fatalf("pairing init error: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", agent.handleHealth)
	mux.HandleFunc("/pairing", agent.handlePairingInfo)
	mux.HandleFunc("/pairing/qr", agent.handlePairingQR)
	mux.HandleFunc("/tunnel/status", agent.handleTunnelStatus)
	mux.HandleFunc("/tunnel/start", agent.handleTunnelStart)
	mux.HandleFunc("/tunnel/stop", agent.handleTunnelStop)
	mux.HandleFunc("/remote/list-dirs", agent.handleListDirs)
	mux.HandleFunc("/ws", agent.handleWS)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("desktop-agent-go listening on %s", cfg.Addr)
	log.Printf("agent_id: %s", store.AgentID())
	log.Printf("default working directory: %s", cfg.DefaultCWD)
	log.Printf("pairing QR image path: %s", cfg.QRPath)
	if cfg.SkipAuth {
		log.Printf("AGENT_SKIP_AUTH=1: auth is bypassed for debugging")
	}
	if cfg.LegacyToken != "" {
		log.Printf("AGENT_TOKEN is set: legacy token auth enabled")
	}
	if cfg.EnforceAccessToken {
		log.Printf("AGENT_ENFORCE_ACCESS_TOKEN=1: access token is required for post-auth messages")
	}
	if len(cfg.AllowedOrigins) > 0 {
		log.Printf("AGENT_ALLOWED_ORIGINS enabled with %d origin(s)", len(cfg.AllowedOrigins))
	}
	log.Printf("cloudflare tunnel target: %s", cfg.TunnelTargetURL)
	if cfg.TunnelAutostart {
		if err := agent.startTunnel(true); err != nil {
			log.Printf("tunnel autostart failed: %v", err)
		}
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server failed: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Printf("shutdown signal received")

	if err := srv.Close(); err != nil {
		log.Printf("server close error: %v", err)
	}
	_ = agent.stopTunnel()
}

func loadConfig() (appConfig, error) {
	addr := getenv("AGENT_ADDR", ":8088")
	defaultCWD := getenv("AGENT_DEFAULT_CWD", mustGetwd())

	statePath := getenv("AGENT_STATE_PATH", "~/.desktop-agent-go/state.json")
	statePath, err := expandHomePath(statePath)
	if err != nil {
		return appConfig{}, err
	}
	stateDir := filepath.Dir(statePath)

	qrPath := getenv("AGENT_QR_PATH", filepath.Join(stateDir, "pairing-qr.png"))
	qrPath, err = expandHomePath(qrPath)
	if err != nil {
		return appConfig{}, err
	}

	pairTTL := defaultPairTTL
	if ttlRaw := strings.TrimSpace(os.Getenv("AGENT_PAIR_TTL_SEC")); ttlRaw != "" {
		var sec int
		if _, err := fmt.Sscanf(ttlRaw, "%d", &sec); err != nil || sec < 30 {
			return appConfig{}, fmt.Errorf("invalid AGENT_PAIR_TTL_SEC: %q", ttlRaw)
		}
		pairTTL = time.Duration(sec) * time.Second
	}

	accessTTL := 30 * time.Minute
	if ttlRaw := strings.TrimSpace(os.Getenv("AGENT_ACCESS_TOKEN_TTL_SEC")); ttlRaw != "" {
		var sec int
		if _, err := fmt.Sscanf(ttlRaw, "%d", &sec); err != nil || sec < 60 {
			return appConfig{}, fmt.Errorf("invalid AGENT_ACCESS_TOKEN_TTL_SEC: %q", ttlRaw)
		}
		accessTTL = time.Duration(sec) * time.Second
	}

	port := getenv("AGENT_PUBLIC_PORT", parseListenPort(addr))
	host := getenv("AGENT_PUBLIC_HOST", detectLocalHost())
	wsScheme := getenv("AGENT_WS_SCHEME", "ws")
	localPort := parseListenPort(addr)
	tunnelTarget := getenv("AGENT_TUNNEL_TARGET_URL", fmt.Sprintf("http://127.0.0.1:%s", localPort))
	allowedOrigins, err := parseAllowedOrigins(os.Getenv("AGENT_ALLOWED_ORIGINS"))
	if err != nil {
		return appConfig{}, err
	}

	cfg := appConfig{
		Addr:               addr,
		DefaultCWD:         defaultCWD,
		LegacyToken:        strings.TrimSpace(os.Getenv("AGENT_TOKEN")),
		ClaudeBin:          strings.TrimSpace(getenv("AGENT_CLAUDE_BIN", "claude")),
		CodexBin:           strings.TrimSpace(getenv("AGENT_CODEX_BIN", "codex")),
		SkipAuth:           getenv("AGENT_SKIP_AUTH", "0") == "1",
		StatePath:          statePath,
		QRPath:             qrPath,
		PairCodeTTL:        pairTTL,
		PublicHost:         host,
		PublicPort:         port,
		WSScheme:           wsScheme,
		EnableQRLogs:       getenv("AGENT_QR_LOG", "1") != "0",
		EnforceAccessToken: getenv("AGENT_ENFORCE_ACCESS_TOKEN", "0") == "1",
		AccessTokenTTL:     accessTTL,
		AllowedOrigins:     allowedOrigins,
		TunnelAutostart:    getenv("AGENT_TUNNEL_AUTOSTART", "0") == "1",
		TunnelBin:          getenv("AGENT_TUNNEL_BIN", "cloudflared"),
		TunnelTargetURL:    tunnelTarget,
	}
	return cfg, nil
}

func (a *agentServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (a *agentServer) handlePairingInfo(w http.ResponseWriter, _ *http.Request) {
	snap, err := a.currentPairing()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	machineIP := a.machineIP()
	tunnel := a.tunnel.Status()
	connectWS := a.preferredWSURL(machineIP)
	payload := map[string]any{
		"agent_id":        a.store.AgentID(),
		"pair_code":       snap.Code,
		"pair_expires_at": snap.ExpiresAt.UTC().Format(time.RFC3339),
		"pairing_ws_url":  snap.WSURL,
		"connect_ws_url":  connectWS,
		"machine_ip":      machineIP,
		"qr_path":         a.cfg.QRPath,
		"tunnel":          tunnel,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func (a *agentServer) handlePairingQR(w http.ResponseWriter, _ *http.Request) {
	snap, err := a.currentPairing()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	png, err := qrcode.Encode(snap.Payload, qrcode.Medium, 320)
	if err != nil {
		http.Error(w, "failed to generate QR", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(png)
}

func (a *agentServer) handleTunnelStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"tunnel": a.tunnel.Status(),
	})
}

func (a *agentServer) handleTunnelStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isLoopbackRequest(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if err := a.startTunnel(false); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":     false,
			"error":  err.Error(),
			"tunnel": a.tunnel.Status(),
		})
		return
	}

	status := a.tunnel.Status()
	code := http.StatusAccepted
	if status.Ready {
		code = http.StatusOK
	}
	writeJSON(w, code, map[string]any{
		"ok":     status.Ready,
		"tunnel": status,
	})
}

func (a *agentServer) handleTunnelStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isLoopbackRequest(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := a.stopTunnel(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"tunnel": a.tunnel.Status(),
	})
}

func (a *agentServer) handleListDirs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	requestedPath := strings.TrimSpace(r.URL.Query().Get("path"))
	resolvedPath, err := resolveCWD(requestedPath, a.cfg.DefaultCWD)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	dirs, err := listDirsByCommand(resolvedPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(listDirsResponse{
		Path: resolvedPath,
		Dirs: dirs,
	})
}

func (a *agentServer) handleWS(w http.ResponseWriter, r *http.Request) {
	if !a.isOriginAllowed(r.Header.Get("Origin")) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade failed: %v", err)
		return
	}

	peer := r.RemoteAddr
	state := &connectionState{
		server:    a,
		conn:      conn,
		sessions:  make(map[string]*session),
		authed:    a.cfg.SkipAuth,
		logPrefix: fmt.Sprintf("[%s]", peer),
	}

	defer func() {
		state.stopAllSessions("connection closed")
		_ = conn.Close()
		log.Printf("%s ws disconnected", state.logPrefix)
	}()

	snap, err := a.currentPairing()
	if err != nil {
		_ = state.sendError("pairing not ready")
		return
	}

	log.Printf("%s ws connected", state.logPrefix)
	_ = state.send(outboundMessage{
		Type:         "ready",
		Message:      "desktop agent connected",
		AgentID:      a.store.AgentID(),
		PairExpires:  snap.ExpiresAt.UTC().Format(time.RFC3339),
		PairingWSURL: snap.WSURL,
	})
	if state.authed {
		accessToken, accessExpire, err := state.issueAccessToken()
		if err != nil {
			_ = state.sendError("failed to issue access token")
			return
		}
		_ = state.send(outboundMessage{
			Type:         "auth_ok",
			Message:      "skip auth enabled",
			AccessToken:  accessToken,
			AccessExpire: accessExpire.UTC().Format(time.RFC3339),
		})
	} else {
		_ = state.send(outboundMessage{Type: "auth_required"})
	}

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("%s read error: %v", state.logPrefix, err)
			}
			return
		}

		var in inboundMessage
		if err := json.Unmarshal(raw, &in); err != nil {
			_ = state.sendError("invalid JSON")
			continue
		}
		if strings.TrimSpace(in.Type) == "" {
			_ = state.sendError("missing message type")
			continue
		}

		if !state.authed && in.Type != "auth" && in.Type != "pair_request" {
			_ = state.sendError("not authenticated")
			continue
		}
		if state.authed && state.server.cfg.EnforceAccessToken && !isAuthFlowMessage(in.Type) {
			if err := state.validateAccessToken(strings.TrimSpace(in.AccessToken)); err != nil {
				_ = state.sendError(err.Error())
				continue
			}
		}

		handleInbound(state, in)
	}
}

func handleInbound(state *connectionState, in inboundMessage) {
	switch in.Type {
	case "auth":
		if err := state.handleAuth(in); err != nil {
			_ = state.sendError(err.Error())
		}
	case "pair_request":
		if err := state.handlePairRequest(in); err != nil {
			_ = state.sendError(err.Error())
		}
	case "start_session":
		if err := state.startSession(in); err != nil {
			_ = state.sendError(err.Error())
		}
	case "input":
		if err := state.writeInput(in.SessionID, in.Data); err != nil {
			_ = state.sendError(err.Error())
		}
	case "interrupt":
		if err := state.interruptSession(in.SessionID); err != nil {
			_ = state.sendError(err.Error())
		}
	case "resize":
		if err := state.resizeSession(in.SessionID, in.Rows, in.Cols); err != nil {
			_ = state.sendError(err.Error())
		}
	case "stop_session":
		if err := state.stopSession(in.SessionID, "requested by client"); err != nil {
			_ = state.sendError(err.Error())
		}
	case "list_sessions":
		state.sessionMu.RLock()
		ids := make([]string, 0, len(state.sessions))
		for id := range state.sessions {
			ids = append(ids, id)
		}
		state.sessionMu.RUnlock()
		sort.Strings(ids)
		_ = state.send(outboundMessage{Type: "sessions", Sessions: ids})
	case "refresh_pairing":
		snap, err := state.server.refreshPairing("manual refresh")
		if err != nil {
			_ = state.sendError(err.Error())
			return
		}
		_ = state.send(outboundMessage{
			Type:         "pairing_refreshed",
			PairCode:     snap.Code,
			PairExpires:  snap.ExpiresAt.UTC().Format(time.RFC3339),
			PairingWSURL: snap.WSURL,
		})
	default:
		_ = state.sendError("unknown message type: " + in.Type)
	}
}

func isAuthFlowMessage(msgType string) bool {
	switch strings.TrimSpace(msgType) {
	case "auth", "pair_request", "refresh_pairing":
		return true
	default:
		return false
	}
}

func (s *connectionState) issueAccessToken() (string, time.Time, error) {
	token, err := randomToken(24)
	if err != nil {
		return "", time.Time{}, err
	}
	expire := time.Now().Add(s.server.cfg.AccessTokenTTL)
	s.accessToken = token
	s.accessExpire = expire
	return token, expire, nil
}

func (s *connectionState) validateAccessToken(in string) error {
	if !s.authed {
		return fmt.Errorf("not authenticated")
	}
	if strings.TrimSpace(in) == "" {
		return fmt.Errorf("missing access_token")
	}
	if s.accessToken == "" || time.Now().After(s.accessExpire) {
		return fmt.Errorf("access_token expired, please auth again")
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(in)), []byte(s.accessToken)) != 1 {
		return fmt.Errorf("invalid access_token")
	}
	return nil
}

func (a *agentServer) isOriginAllowed(origin string) bool {
	if len(a.cfg.AllowedOrigins) == 0 {
		return true
	}
	trimmed := strings.TrimSpace(origin)
	if trimmed == "" {
		return false
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Host))
	if host == "" {
		return false
	}
	_, ok := a.cfg.AllowedOrigins[host]
	return ok
}

func (s *connectionState) handleAuth(in inboundMessage) error {
	if s.server.cfg.SkipAuth {
		s.authed = true
		accessToken, accessExpire, err := s.issueAccessToken()
		if err != nil {
			return fmt.Errorf("failed to issue access token: %w", err)
		}
		return s.send(outboundMessage{
			Type:         "auth_ok",
			Message:      "skip auth enabled",
			AgentID:      s.server.store.AgentID(),
			AccessToken:  accessToken,
			AccessExpire: accessExpire.UTC().Format(time.RFC3339),
		})
	}

	legacyToken := s.server.cfg.LegacyToken
	if legacyToken != "" && strings.TrimSpace(in.Token) != "" {
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(in.Token)), []byte(legacyToken)) == 1 {
			s.authed = true
			accessToken, accessExpire, err := s.issueAccessToken()
			if err != nil {
				return fmt.Errorf("failed to issue access token: %w", err)
			}
			return s.send(outboundMessage{
				Type:         "auth_ok",
				Message:      "legacy token auth ok",
				AgentID:      s.server.store.AgentID(),
				AccessToken:  accessToken,
				AccessExpire: accessExpire.UTC().Format(time.RFC3339),
			})
		}
	}

	deviceID := strings.TrimSpace(in.DeviceID)
	deviceToken := strings.TrimSpace(in.Token)
	if deviceID == "" || deviceToken == "" {
		return fmt.Errorf("missing device_id or token")
	}

	ok, err := s.server.store.AuthenticateDevice(deviceID, deviceToken)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("device auth failed")
	}

	s.authed = true
	s.deviceID = deviceID
	accessToken, accessExpire, err := s.issueAccessToken()
	if err != nil {
		return fmt.Errorf("failed to issue access token: %w", err)
	}
	return s.send(outboundMessage{
		Type:         "auth_ok",
		DeviceID:     deviceID,
		AgentID:      s.server.store.AgentID(),
		AccessToken:  accessToken,
		AccessExpire: accessExpire.UTC().Format(time.RFC3339),
	})
}

func (s *connectionState) handlePairRequest(in inboundMessage) error {
	snap, err := s.server.currentPairing()
	if err != nil {
		return err
	}

	inCode := normalizePairCode(in.PairCode)
	srvCode := normalizePairCode(snap.Code)
	if inCode == "" {
		return fmt.Errorf("missing pair_code")
	}
	if subtle.ConstantTimeCompare([]byte(inCode), []byte(srvCode)) != 1 {
		return fmt.Errorf("invalid pair_code")
	}
	if time.Now().After(snap.ExpiresAt) {
		_, _ = s.server.refreshPairing("pair code expired")
		return fmt.Errorf("pair_code expired, please rescan QR")
	}

	deviceID := strings.TrimSpace(in.DeviceID)
	if deviceID == "" {
		return fmt.Errorf("missing device_id")
	}
	deviceName := strings.TrimSpace(in.DeviceName)
	if deviceName == "" {
		deviceName = "unknown-device"
	}

	deviceToken, err := randomToken(24)
	if err != nil {
		return fmt.Errorf("failed to generate device token: %w", err)
	}

	if err := s.server.store.UpsertDevice(deviceID, deviceName, deviceToken); err != nil {
		return err
	}

	s.authed = true
	s.deviceID = deviceID
	accessToken, accessExpire, err := s.issueAccessToken()
	if err != nil {
		return fmt.Errorf("failed to issue access token: %w", err)
	}

	_ = s.send(outboundMessage{
		Type:         "pair_success",
		Message:      "device paired successfully",
		DeviceID:     deviceID,
		DeviceToken:  deviceToken,
		AgentID:      s.server.store.AgentID(),
		AccessToken:  accessToken,
		AccessExpire: accessExpire.UTC().Format(time.RFC3339),
	})
	_ = s.send(outboundMessage{
		Type:         "auth_ok",
		DeviceID:     deviceID,
		AgentID:      s.server.store.AgentID(),
		AccessToken:  accessToken,
		AccessExpire: accessExpire.UTC().Format(time.RFC3339),
	})

	_, _ = s.server.refreshPairing("paired device " + deviceID)
	return nil
}

func (s *connectionState) startSession(in inboundMessage) error {
	tool := strings.ToLower(strings.TrimSpace(in.Tool))
	defaultExecName, ok := allowedTool(tool)
	if !ok {
		return fmt.Errorf("unsupported tool %q, allowed: claude, codex", in.Tool)
	}
	execName := s.server.toolExecName(tool, defaultExecName)

	sessionID := strings.TrimSpace(in.SessionID)
	if sessionID == "" {
		sessionID = newSessionID()
	}

	cwd, err := resolveCWD(in.CWD, s.server.cfg.DefaultCWD)
	if err != nil {
		return err
	}

	s.sessionMu.Lock()
	if _, exists := s.sessions[sessionID]; exists {
		s.sessionMu.Unlock()
		return fmt.Errorf("session already exists: %s", sessionID)
	}

	if tool == "claude" {
		model, resumeID := parseClaudeSessionArgs(in.Args)
		sess := &session{
			id:       sessionID,
			tool:     tool,
			cwd:      cwd,
			model:    model,
			claudeID: resumeID,
			stopped:  make(chan struct{}),
		}
		s.sessions[sessionID] = sess
		s.sessionMu.Unlock()

		_ = s.send(outboundMessage{
			Type:      "session_started",
			SessionID: sessionID,
			Tool:      tool,
			CWD:       cwd,
			Message:   "claude session ready",
		})
		if resumeID != "" {
			_ = s.send(outboundMessage{
				Type:      "claude_session_id",
				SessionID: sessionID,
				Data:      resumeID,
			})
		}
		return nil
	}

	args := buildToolArgs(tool, in.Args)
	cmd := exec.Command(execName, args...)
	cmd.Dir = cwd
	cmd.Env = withTerminalEnv(os.Environ())

	stdin, err := cmd.StdinPipe()
	if err != nil {
		s.sessionMu.Unlock()
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.sessionMu.Unlock()
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.sessionMu.Unlock()
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		s.sessionMu.Unlock()
		return fmt.Errorf("failed to start %s: %s", execName, formatToolStartError(tool, execName, err))
	}

	sess := &session{
		id:      sessionID,
		tool:    tool,
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		stopped: make(chan struct{}),
	}
	s.sessions[sessionID] = sess
	s.sessionMu.Unlock()

	_ = s.send(outboundMessage{
		Type:      "session_started",
		SessionID: sessionID,
		Tool:      tool,
		CWD:       cwd,
		Message:   fmt.Sprintf("%s started", execName),
	})

	go s.streamOutput(sess, "stdout", sess.stdout)
	go s.streamOutput(sess, "stderr", sess.stderr)
	go s.waitSessionExit(sess)
	return nil
}

func parseClaudeSessionArgs(args []string) (model string, resumeID string) {
	model = "default"
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		switch arg {
		case "--model":
			if i+1 < len(args) {
				model = strings.TrimSpace(args[i+1])
				i++
			}
		case "--resume":
			if i+1 < len(args) {
				resumeID = strings.TrimSpace(args[i+1])
				i++
			}
		}
	}
	if model == "" {
		model = "default"
	}
	return model, resumeID
}

func buildToolArgs(tool string, clientArgs []string) []string {
	if tool == "claude" {
		return nil
	}
	return append([]string{}, clientArgs...)
}

func (s *connectionState) streamOutput(sess *session, stream string, r io.ReadCloser) {
	defer func() { _ = r.Close() }()

	if sess.tool == "claude" && stream == "stdout" {
		s.streamClaudeJSON(sess, r)
		return
	}

	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_ = s.send(outboundMessage{
				Type:      "output",
				SessionID: sess.id,
				Stream:    stream,
				Data:      string(buf[:n]),
			})
		}
		if err != nil {
			if !errors.Is(err, os.ErrClosed) && !errors.Is(err, io.EOF) {
				_ = s.sendError(fmt.Sprintf("session %s read error: %v", sess.id, err))
			}
			return
		}
	}
}

func (s *connectionState) streamClaudeJSON(sess *session, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			_ = s.send(outboundMessage{
				Type:      "output",
				SessionID: sess.id,
				Stream:    "stdout",
				Data:      line + "\n",
			})
			continue
		}

		if sid, ok := payload["session_id"].(string); ok && strings.TrimSpace(sid) != "" {
			sess.mu.Lock()
			sess.claudeID = sid
			sess.mu.Unlock()
			_ = s.send(outboundMessage{
				Type:      "claude_session_id",
				SessionID: sess.id,
				Data:      sid,
			})
		}

		for _, chunk := range extractClaudeTextChunks(payload) {
			if strings.TrimSpace(chunk) == "" {
				continue
			}
			_ = s.send(outboundMessage{
				Type:      "output",
				SessionID: sess.id,
				Stream:    "stdout",
				Data:      chunk,
			})
		}
	}

	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		_ = s.sendError(fmt.Sprintf("session %s scan error: %v", sess.id, err))
	}
}

func extractClaudeTextChunks(payload map[string]any) []string {
	msgType, _ := payload["type"].(string)
	switch msgType {
	case "user":
		return nil
	case "system":
		// System init/update events are metadata for backend state sync only.
		return nil
	case "stream_event":
		event, ok := payload["event"].(map[string]any)
		if !ok {
			return nil
		}
		eventType, _ := event["type"].(string)
		if eventType == "content_block_delta" {
			if delta, ok := event["delta"].(map[string]any); ok {
				if text, ok := delta["text"].(string); ok && strings.TrimSpace(text) != "" {
					return []string{text}
				}
				if text, ok := delta["text_delta"].(string); ok && strings.TrimSpace(text) != "" {
					return []string{text}
				}
			}
		}
		return nil
	}

	var out []string
	if result, ok := payload["result"].(string); ok && strings.TrimSpace(result) != "" {
		out = append(out, result)
	}
	if text, ok := payload["text"].(string); ok && strings.TrimSpace(text) != "" {
		out = append(out, text)
	}
	if delta, ok := payload["delta"].(map[string]any); ok {
		if text, ok := delta["text"].(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, text)
		}
	}
	if msg, ok := payload["message"].(map[string]any); ok {
		out = append(out, extractClaudeMessageText(msg)...)
	}
	return out
}

func extractClaudeMessageText(message map[string]any) []string {
	var out []string
	if text, ok := message["text"].(string); ok && strings.TrimSpace(text) != "" {
		out = append(out, text)
	}
	content, ok := message["content"].([]any)
	if !ok {
		return out
	}
	for _, item := range content {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := m["text"].(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, text)
		}
	}
	return out
}

func withTerminalEnv(env []string) []string {
	out := make([]string, 0, len(env)+3)
	hasTERM := false
	hasColor := false
	hasPath := false

	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "TERM="):
			out = append(out, "TERM=xterm-256color")
			hasTERM = true
		case strings.HasPrefix(kv, "COLORTERM="):
			out = append(out, "COLORTERM=truecolor")
			hasColor = true
		case strings.HasPrefix(kv, "PATH="):
			rawPath := strings.TrimPrefix(kv, "PATH=")
			out = append(out, "PATH="+ensureCommandPath(rawPath))
			hasPath = true
		default:
			out = append(out, kv)
		}
	}

	if !hasTERM {
		out = append(out, "TERM=xterm-256color")
	}
	if !hasColor {
		out = append(out, "COLORTERM=truecolor")
	}
	if !hasPath {
		out = append(out, "PATH="+ensureCommandPath(""))
	}
	return out
}

func ensureCommandPath(current string) string {
	ordered := make([]string, 0, 16)
	seen := make(map[string]struct{}, 16)
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		p = filepath.Clean(p)
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		ordered = append(ordered, p)
	}

	for _, p := range filepath.SplitList(current) {
		add(p)
	}

	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		add(filepath.Join(home, ".local", "bin"))
		add(filepath.Join(home, ".npm", "bin"))
		add(filepath.Join(home, "bin"))
		matches, _ := filepath.Glob(filepath.Join(home, ".nvm", "versions", "node", "*", "bin"))
		if len(matches) > 0 {
			sort.Strings(matches)
			add(matches[len(matches)-1])
		}
	}

	add("/usr/local/bin")
	add("/usr/bin")
	add("/bin")
	add("/usr/sbin")
	add("/sbin")
	add("/snap/bin")

	return strings.Join(ordered, string(os.PathListSeparator))
}

func (s *connectionState) waitSessionExit(sess *session) {
	err := sess.cmd.Wait()
	exitCode := exitCodeFromError(err)

	s.removeSession(sess.id)
	sess.stopOnce.Do(func() {
		if sess.stdin != nil {
			_ = sess.stdin.Close()
		}
		if sess.stdout != nil {
			_ = sess.stdout.Close()
		}
		if sess.stderr != nil {
			_ = sess.stderr.Close()
		}
		close(sess.stopped)
	})

	_ = s.send(outboundMessage{
		Type:      "session_exit",
		SessionID: sess.id,
		ExitCode:  &exitCode,
		Message:   "process exited",
	})
}

func (s *connectionState) writeInput(sessionID, data string) error {
	sess, err := s.getSession(sessionID)
	if err != nil {
		return err
	}
	if data == "" {
		return nil
	}

	if sess.tool == "claude" {
		input := strings.TrimSpace(strings.TrimRight(data, "\r\n"))
		if input == "" {
			return nil
		}
		if input == "/exit" {
			return s.stopSession(sessionID, "requested by /exit")
		}
		if strings.HasPrefix(input, "/model") {
			parts := strings.Fields(input)
			if len(parts) >= 2 {
				model := strings.TrimSpace(parts[1])
				if model == "" {
					model = "default"
				}
				sess.mu.Lock()
				sess.model = model
				sess.mu.Unlock()
				_ = s.send(outboundMessage{
					Type:      "output",
					SessionID: sessionID,
					Stream:    "stdout",
					Data:      "模型已切换为: " + model,
				})
			}
			return nil
		}

		sess.mu.Lock()
		if sess.running {
			sess.mu.Unlock()
			return fmt.Errorf("claude session %s is busy", sessionID)
		}
		sess.running = true
		sess.mu.Unlock()

		go s.runClaudeTurn(sess, input)
		return nil
	}

	_, err = io.WriteString(sess.stdin, data)
	if err != nil {
		return fmt.Errorf("write failed for session %s: %w", sessionID, err)
	}
	return nil
}

func (s *connectionState) runClaudeTurn(sess *session, prompt string) {
	defer func() {
		sess.mu.Lock()
		sess.running = false
		sess.runCmd = nil
		sess.mu.Unlock()
	}()

	sess.mu.RLock()
	cwd := sess.cwd
	resumeID := strings.TrimSpace(sess.claudeID)
	model := strings.TrimSpace(sess.model)
	sess.mu.RUnlock()

	args := []string{
		"--print",
		"--verbose",
		"--output-format", "stream-json",
		"--include-partial-messages",
	}
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	if model != "" && model != "default" {
		args = append(args, "--model", model)
	}
	args = append(args, prompt)

	execName := s.server.toolExecName("claude", "claude")
	cmd := exec.Command(execName, args...)
	cmd.Dir = cwd
	cmd.Env = withTerminalEnv(os.Environ())

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = s.sendError(fmt.Sprintf("claude stdout pipe failed: %v", err))
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = s.sendError(fmt.Sprintf("claude stderr pipe failed: %v", err))
		return
	}
	if err := cmd.Start(); err != nil {
		_ = s.sendError(formatToolStartError("claude", execName, err))
		return
	}

	sess.mu.Lock()
	sess.runCmd = cmd
	sess.mu.Unlock()

	type scanResult struct {
		hasText bool
		err     error
	}
	stdoutDone := make(chan scanResult, 1)
	stderrDone := make(chan error, 1)

	go func() {
		hasText, scanErr := s.streamClaudeJSONTurn(sess, stdout)
		stdoutDone <- scanResult{hasText: hasText, err: scanErr}
	}()
	go func() {
		stderrDone <- s.streamPipeLines(sess.id, "stderr", stderr)
	}()

	waitErr := cmd.Wait()
	outcome := <-stdoutDone
	errStderr := <-stderrDone

	if outcome.err != nil {
		_ = s.sendError(fmt.Sprintf("claude output parse failed: %v", outcome.err))
	}
	if errStderr != nil {
		_ = s.sendError(fmt.Sprintf("claude stderr read failed: %v", errStderr))
	}
	if waitErr != nil {
		exitCode := exitCodeFromError(waitErr)
		if exitCode == 143 || exitCode == 130 {
			return
		}
		if _, err := s.getSession(sess.id); err != nil {
			return
		}
		_ = s.send(outboundMessage{
			Type:      "output",
			SessionID: sess.id,
			Stream:    "stderr",
			Data:      fmt.Sprintf("Claude 命令执行失败，退出码 %d", exitCode),
		})
	}
}

func (s *connectionState) streamPipeLines(sessionID, stream string, r io.ReadCloser) error {
	defer func() { _ = r.Close() }()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		_ = s.send(outboundMessage{
			Type:      "output",
			SessionID: sessionID,
			Stream:    stream,
			Data:      line,
		})
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (s *connectionState) streamClaudeJSONTurn(sess *session, r io.ReadCloser) (bool, error) {
	defer func() { _ = r.Close() }()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	seenText := false
	seenDeltaText := false
	seenAssistantText := false
	lastChunk := ""
	lastTrace := ""
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			continue
		}
		if sid, ok := payload["session_id"].(string); ok && strings.TrimSpace(sid) != "" {
			changed := false
			sess.mu.Lock()
			if sess.claudeID != sid {
				sess.claudeID = sid
				changed = true
			}
			sess.mu.Unlock()
			if changed {
				_ = s.send(outboundMessage{
					Type:      "claude_session_id",
					SessionID: sess.id,
					Data:      sid,
				})
			}
		}

		msgType, _ := payload["type"].(string)
		if msgType == "stream_event" {
			event, _ := payload["event"].(map[string]any)
			eventType, _ := event["type"].(string)
			if eventType == "content_block_delta" {
				seenDeltaText = true
			}
		}
		// Once delta chunks have been emitted, skip assistant/result rollups to prevent duplicate text.
		if msgType == "assistant" && seenDeltaText {
			continue
		}
		if msgType == "result" && (seenDeltaText || seenAssistantText) {
			continue
		}

		for _, trace := range extractClaudeTraceEvents(payload) {
			trace = strings.TrimSpace(trace)
			if trace == "" || trace == lastTrace {
				continue
			}
			lastTrace = trace
			_ = s.send(outboundMessage{
				Type:      "trace",
				SessionID: sess.id,
				Stream:    "stdout",
				Data:      trace,
			})
		}

		for _, chunk := range extractClaudeTextChunks(payload) {
			if strings.TrimSpace(chunk) == "" {
				continue
			}
			if chunk == lastChunk {
				continue
			}
			lastChunk = chunk
			seenText = true
			if msgType == "assistant" {
				seenAssistantText = true
			}
			_ = s.send(outboundMessage{
				Type:      "output",
				SessionID: sess.id,
				Stream:    "stdout",
				Data:      chunk,
			})
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return seenText, err
	}
	return seenText, nil
}

func extractClaudeTraceEvents(payload map[string]any) []string {
	msgType, _ := payload["type"].(string)
	if msgType != "stream_event" {
		return nil
	}
	event, ok := payload["event"].(map[string]any)
	if !ok {
		return nil
	}
	eventType, _ := event["type"].(string)
	if eventType != "content_block_start" {
		return nil
	}
	block, ok := event["content_block"].(map[string]any)
	if !ok {
		return nil
	}
	blockType, _ := block["type"].(string)
	switch strings.TrimSpace(blockType) {
	case "thinking":
		return []string{"思考中..."}
	case "tool_use":
		toolName := firstNonEmptyString(
			stringFromAny(block["name"]),
			stringFromAny(block["tool_name"]),
		)
		if toolName == "" {
			return []string{"调用工具"}
		}
		return []string{fmt.Sprintf("调用工具: %s", toolName)}
	case "web_search_tool_result":
		return []string{"检索结果返回"}
	default:
		return nil
	}
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		t := strings.TrimSpace(v)
		if t != "" {
			return t
		}
	}
	return ""
}

func stringFromAny(v any) string {
	s, _ := v.(string)
	return s
}

func (s *connectionState) interruptSession(sessionID string) error {
	sess, err := s.getSession(sessionID)
	if err != nil {
		return err
	}
	if sess.tool == "claude" {
		sess.mu.RLock()
		run := sess.runCmd
		sess.mu.RUnlock()
		if run != nil && run.Process != nil {
			if err := run.Process.Signal(os.Interrupt); err != nil {
				return fmt.Errorf("interrupt failed for session %s: %w", sessionID, err)
			}
		}
		return s.send(outboundMessage{Type: "interrupted", SessionID: sessionID})
	}
	if sess.cmd.Process == nil {
		return fmt.Errorf("session %s has no process", sessionID)
	}
	err = sess.cmd.Process.Signal(os.Interrupt)
	if err != nil {
		return fmt.Errorf("interrupt failed for session %s: %w", sessionID, err)
	}
	return s.send(outboundMessage{Type: "interrupted", SessionID: sessionID})
}

func (s *connectionState) resizeSession(sessionID string, rows, cols uint16) error {
	if _, err := s.getSession(sessionID); err != nil {
		return err
	}
	if rows == 0 || cols == 0 {
		return fmt.Errorf("rows and cols must be > 0")
	}
	return s.send(outboundMessage{Type: "resized", SessionID: sessionID})
}

func (s *connectionState) stopSession(sessionID, reason string) error {
	sess, err := s.getSession(sessionID)
	if err != nil {
		return err
	}
	if sess.tool == "claude" {
		s.removeSession(sessionID)
		sess.mu.RLock()
		run := sess.runCmd
		sess.mu.RUnlock()
		if run != nil && run.Process != nil {
			_ = run.Process.Signal(syscall.SIGTERM)
		}
		sess.stopOnce.Do(func() {
			close(sess.stopped)
		})
		exitCode := 0
		_ = s.send(outboundMessage{
			Type:      "session_exit",
			SessionID: sessionID,
			ExitCode:  &exitCode,
			Message:   reason,
		})
		return nil
	}
	if sess.cmd.Process == nil {
		return fmt.Errorf("session %s has no process", sessionID)
	}

	if err := sess.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("failed to stop session %s: %w", sessionID, err)
	}
	if sess.stdin != nil {
		_ = sess.stdin.Close()
	}
	_ = s.send(outboundMessage{Type: "stopping", SessionID: sessionID, Message: reason})

	go func() {
		select {
		case <-sess.stopped:
			return
		case <-time.After(2 * time.Second):
			_ = sess.cmd.Process.Kill()
		}
	}()
	return nil
}

func (s *connectionState) stopAllSessions(reason string) {
	s.sessionMu.RLock()
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	s.sessionMu.RUnlock()

	for _, id := range ids {
		_ = s.stopSession(id, reason)
	}
}

func (s *connectionState) getSession(id string) (*session, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("missing session_id")
	}
	s.sessionMu.RLock()
	sess, ok := s.sessions[id]
	s.sessionMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session not found: %s", id)
	}
	return sess, nil
}

func (s *connectionState) removeSession(id string) {
	s.sessionMu.Lock()
	delete(s.sessions, id)
	s.sessionMu.Unlock()
}

func (s *connectionState) sendError(message string) error {
	return s.send(outboundMessage{Type: "error", Message: message})
}

func (s *connectionState) send(msg outboundMessage) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if msg.At == "" {
		msg.At = nowISO()
	}
	if err := s.conn.SetWriteDeadline(time.Now().Add(8 * time.Second)); err != nil {
		return err
	}
	return s.conn.WriteJSON(msg)
}

func (a *agentServer) currentPairing() (pairingSnapshot, error) {
	a.pairMu.RLock()
	code := a.pairCode
	expires := a.pairExpires
	a.pairMu.RUnlock()

	if code == "" || time.Now().After(expires) {
		return a.refreshPairing("auto refresh")
	}
	return a.buildPairingSnapshot(code, expires)
}

func (a *agentServer) refreshPairing(reason string) (pairingSnapshot, error) {
	code, err := newPairCode()
	if err != nil {
		return pairingSnapshot{}, err
	}
	expires := time.Now().Add(a.cfg.PairCodeTTL)

	a.pairMu.Lock()
	a.pairCode = code
	a.pairExpires = expires
	a.pairMu.Unlock()

	snap, err := a.buildPairingSnapshot(code, expires)
	if err != nil {
		return pairingSnapshot{}, err
	}

	if err := qrcode.WriteFile(snap.Payload, qrcode.Medium, 320, a.cfg.QRPath); err != nil {
		log.Printf("failed to write QR file: %v", err)
	}

	if a.cfg.EnableQRLogs {
		a.printPairingQR(snap, reason)
	}
	return snap, nil
}

func (a *agentServer) buildPairingSnapshot(code string, expires time.Time) (pairingSnapshot, error) {
	machineIP := a.machineIP()
	pairingWS := a.preferredWSURL(machineIP)
	payloadMap := map[string]string{
		"type":         "desktop_agent_pair",
		"agent_id":     a.store.AgentID(),
		"pair_code":    code,
		"pairing_ws":   pairingWS,
		"connect_ws":   pairingWS,
		"machine_ip":   machineIP,
		"expires_at":   expires.UTC().Format(time.RFC3339),
		"display_name": mustHostname(),
	}
	raw, err := json.Marshal(payloadMap)
	if err != nil {
		return pairingSnapshot{}, err
	}

	return pairingSnapshot{
		Code:      code,
		ExpiresAt: expires,
		Payload:   string(raw),
		WSURL:     pairingWS,
	}, nil
}

func (a *agentServer) printPairingQR(snap pairingSnapshot, reason string) {
	qr, err := qrcode.New(snap.Payload, qrcode.Medium)
	if err != nil {
		log.Printf("pairing refreshed (%s): code=%s expires=%s ws=%s", reason, snap.Code, snap.ExpiresAt.Format(time.RFC3339), snap.WSURL)
		return
	}

	fmt.Println()
	fmt.Println("==================================================")
	fmt.Println("Scan this QR in your mobile app to pair this device")
	fmt.Println(qr.ToString(false))
	fmt.Printf("Pair code: %s\n", snap.Code)
	fmt.Printf("Expires : %s\n", snap.ExpiresAt.Local().Format(time.RFC3339))
	fmt.Printf("WS URL  : %s\n", snap.WSURL)
	fmt.Printf("QR file : %s\n", a.cfg.QRPath)
	fmt.Printf("Reason  : %s\n", reason)
	fmt.Println("==================================================")
	fmt.Println()
}

func (a *agentServer) startTunnel(quiet bool) error {
	if a.tunnel == nil {
		return fmt.Errorf("tunnel manager not initialized")
	}
	if err := a.tunnel.Start(15 * time.Second); err != nil {
		return err
	}
	status := a.tunnel.Status()
	if status.Ready && !quiet {
		log.Printf("cloudflare tunnel ready: %s", status.WSURL)
	}
	if _, err := a.refreshPairing("tunnel start"); err != nil {
		log.Printf("pairing refresh after tunnel start failed: %v", err)
	}
	return nil
}

func (a *agentServer) stopTunnel() error {
	if a.tunnel == nil {
		return nil
	}
	err := a.tunnel.Stop()
	if _, refreshErr := a.refreshPairing("tunnel stop"); refreshErr != nil {
		log.Printf("pairing refresh after tunnel stop failed: %v", refreshErr)
	}
	return err
}

func (a *agentServer) preferredWSURL(machineIP string) string {
	if a.tunnel != nil {
		status := a.tunnel.Status()
		if status.Ready && status.WSURL != "" {
			return status.WSURL
		}
	}
	return a.pairingWSURL(machineIP)
}

func (a *agentServer) wsURL() string {
	return fmt.Sprintf("%s://%s:%s/ws", a.cfg.WSScheme, a.cfg.PublicHost, a.cfg.PublicPort)
}

func (a *agentServer) wsURLForHost(host string) string {
	return fmt.Sprintf("%s://%s:%s/ws", a.cfg.WSScheme, host, a.cfg.PublicPort)
}

func (a *agentServer) pairingWSURL(machineIP string) string {
	if machineIP != "" && machineIP != "127.0.0.1" && isLoopbackHost(a.cfg.PublicHost) {
		return a.wsURLForHost(machineIP)
	}
	return a.wsURL()
}

func (a *agentServer) machineIP() string {
	ip := detectLocalHost()
	if ip != "" && ip != "127.0.0.1" {
		return ip
	}
	if !isLoopbackHost(a.cfg.PublicHost) {
		return a.cfg.PublicHost
	}
	return ip
}

var tryCloudflareURLPattern = regexp.MustCompile(`https://[a-zA-Z0-9.-]+\.trycloudflare\.com`)

func newTunnelManager(bin, targetURL string) *tunnelManager {
	return &tunnelManager{
		bin:       strings.TrimSpace(bin),
		targetURL: strings.TrimSpace(targetURL),
	}
}

func (t *tunnelManager) Start(waitReady time.Duration) error {
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return t.waitUntilReady(waitReady)
	}
	bin := t.bin
	if bin == "" {
		bin = "cloudflared"
	}
	targetURL := t.targetURL
	if targetURL == "" {
		targetURL = "http://127.0.0.1:8088"
	}
	t.mu.Unlock()

	resolved, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("cloudflared not found, install first: %w", err)
	}

	cmd := exec.Command(resolved, "tunnel", "--url", targetURL, "--no-autoupdate")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("create tunnel stdout pipe failed: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("create tunnel stderr pipe failed: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start cloudflared failed: %w", err)
	}

	t.mu.Lock()
	t.cmd = cmd
	t.running = true
	t.publicURL = ""
	t.wsURL = ""
	t.lastError = ""
	t.startedAt = time.Now()
	t.mu.Unlock()

	log.Printf("cloudflared tunnel starting pid=%d target=%s", cmd.Process.Pid, targetURL)
	go t.consumeStream(stdout, "stdout")
	go t.consumeStream(stderr, "stderr")
	go t.waitProcess(cmd)

	return t.waitUntilReady(waitReady)
}

func (t *tunnelManager) waitUntilReady(waitReady time.Duration) error {
	if waitReady <= 0 {
		return nil
	}
	deadline := time.Now().Add(waitReady)
	for {
		status := t.Status()
		if status.Ready {
			return nil
		}
		if !status.Enabled {
			if status.LastError != "" {
				return fmt.Errorf(status.LastError)
			}
			return fmt.Errorf("cloudflared is not running")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cloudflared started but tunnel URL is not ready yet")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (t *tunnelManager) consumeStream(reader io.Reader, stream string) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		if u := extractTryCloudflareURL(line); u != "" {
			t.mu.Lock()
			t.publicURL = u
			t.wsURL = toWSURL(u)
			t.lastError = ""
			t.mu.Unlock()
			log.Printf("cloudflared tunnel ready: %s", t.wsURL)
		}
		if strings.Contains(strings.ToLower(line), "error") {
			t.mu.Lock()
			t.lastError = fmt.Sprintf("cloudflared %s: %s", stream, strings.TrimSpace(line))
			t.mu.Unlock()
		}
	}
	if err := scanner.Err(); err != nil {
		t.mu.Lock()
		t.lastError = fmt.Sprintf("cloudflared %s scanner error: %v", stream, err)
		t.mu.Unlock()
	}
}

func (t *tunnelManager) waitProcess(cmd *exec.Cmd) {
	err := cmd.Wait()

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cmd != cmd {
		return
	}
	t.running = false
	t.cmd = nil
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.lastError = fmt.Sprintf("cloudflared exited: %v", err)
		log.Printf("cloudflared exited with error: %v", err)
		return
	}
	log.Printf("cloudflared exited")
}

func (t *tunnelManager) Stop() error {
	t.mu.Lock()
	cmd := t.cmd
	t.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("stop cloudflared failed: %w", err)
		}
	}

	t.mu.Lock()
	t.cmd = nil
	t.running = false
	t.publicURL = ""
	t.wsURL = ""
	t.startedAt = time.Time{}
	t.lastError = ""
	t.mu.Unlock()
	log.Printf("cloudflared stopped")
	return nil
}

func (t *tunnelManager) Status() tunnelStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := tunnelStatus{
		Enabled:   t.running,
		Ready:     t.running && t.wsURL != "",
		PublicURL: t.publicURL,
		WSURL:     t.wsURL,
		LastError: t.lastError,
	}
	if !t.startedAt.IsZero() {
		out.StartedAt = t.startedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func extractTryCloudflareURL(line string) string {
	matched := tryCloudflareURLPattern.FindString(line)
	if matched == "" {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(matched))
	if err != nil || u.Host == "" {
		return ""
	}
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func toWSURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = "/ws"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func listDirsByCommand(path string) ([]remoteDirItem, error) {
	cmd := exec.Command("find", path, "-mindepth", "1", "-maxdepth", "1", "-type", "d")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list dirs by command: %w: %s", err, strings.TrimSpace(string(out)))
	}

	lines := strings.Split(string(out), "\n")
	dirs := make([]remoteDirItem, 0, len(lines))
	for _, line := range lines {
		abs := strings.TrimSpace(line)
		if abs == "" {
			continue
		}
		dirs = append(dirs, remoteDirItem{
			Name: filepath.Base(abs),
			Path: abs,
		})
	}

	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name)
	})
	return dirs, nil
}

func newStateStore(path string) (*stateStore, error) {
	st := &stateStore{
		path: path,
		data: persistentState{PairedDevices: map[string]pairedDevice{}},
	}
	if err := st.loadLocked(); err != nil {
		return nil, err
	}
	if st.data.AgentID == "" {
		agentID, err := randomToken(12)
		if err != nil {
			return nil, err
		}
		st.data.AgentID = agentID
		if err := st.saveLocked(); err != nil {
			return nil, err
		}
	}
	if st.data.PairedDevices == nil {
		st.data.PairedDevices = map[string]pairedDevice{}
		if err := st.saveLocked(); err != nil {
			return nil, err
		}
	}
	return st, nil
}

func (s *stateStore) AgentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.AgentID
}

func (s *stateStore) UpsertDevice(deviceID, deviceName, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := nowISO()
	existing, ok := s.data.PairedDevices[deviceID]
	pairedAt := now
	if ok && existing.PairedAt != "" {
		pairedAt = existing.PairedAt
	}

	s.data.PairedDevices[deviceID] = pairedDevice{
		DeviceName: deviceName,
		Token:      token,
		PairedAt:   pairedAt,
		LastSeenAt: now,
	}
	return s.saveLocked()
}

func (s *stateStore) AuthenticateDevice(deviceID, token string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	dev, ok := s.data.PairedDevices[deviceID]
	if !ok {
		return false, nil
	}
	if subtle.ConstantTimeCompare([]byte(dev.Token), []byte(token)) != 1 {
		return false, nil
	}

	dev.LastSeenAt = nowISO()
	s.data.PairedDevices[deviceID] = dev
	if err := s.saveLocked(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *stateStore) loadLocked() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("failed to read state file: %w", err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil
	}

	var loaded persistentState
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return fmt.Errorf("failed to parse state file: %w", err)
	}
	if loaded.PairedDevices == nil {
		loaded.PairedDevices = map[string]pairedDevice{}
	}
	s.data = loaded
	return nil
}

func (s *stateStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("failed to create state dir: %w", err)
	}

	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize state: %w", err)
	}
	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, raw, 0o600); err != nil {
		return fmt.Errorf("failed to write temp state: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("failed to save state: %w", err)
	}
	return nil
}

func resolveCWD(requested, defaultCWD string) (string, error) {
	cwd := strings.TrimSpace(requested)
	if cwd == "" {
		cwd = defaultCWD
	}
	if !filepath.IsAbs(cwd) {
		abs, err := filepath.Abs(cwd)
		if err != nil {
			return "", fmt.Errorf("invalid cwd %q: %w", cwd, err)
		}
		cwd = abs
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return "", fmt.Errorf("cwd not accessible: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("cwd is not a directory: %s", cwd)
	}
	return cwd, nil
}

func allowedTool(tool string) (string, bool) {
	switch tool {
	case "claude":
		return "claude", true
	case "codex":
		return "codex", true
	default:
		return "", false
	}
}

func (a *agentServer) toolExecName(tool, fallback string) string {
	switch tool {
	case "claude":
		if v := strings.TrimSpace(a.cfg.ClaudeBin); v != "" {
			return v
		}
	case "codex":
		if v := strings.TrimSpace(a.cfg.CodexBin); v != "" {
			return v
		}
	}
	return strings.TrimSpace(fallback)
}

func formatToolStartError(tool, execName string, err error) string {
	base := fmt.Sprintf("%s start failed: %v", tool, err)
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "executable file not found") || strings.Contains(lower, "no such file or directory") {
		envKey := "AGENT_" + strings.ToUpper(tool) + "_BIN"
		return fmt.Sprintf("%s. 未找到可执行文件 %q；请在 agent.env 配置 %s=/绝对路径/%s（可用 `which %s` 获取），然后重启服务。", base, execName, envKey, tool, tool)
	}
	if strings.Contains(lower, "permission denied") {
		return fmt.Sprintf("%s. 可执行文件权限不足，请执行 `chmod +x %s` 后重试。", base, execName)
	}
	return base
}

func exitCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return 128 + int(status.Signal())
			}
			return status.ExitStatus()
		}
	}
	return 1
}

func newSessionID() string {
	n := atomic.AddUint64(&sessionCounter, 1)
	return fmt.Sprintf("s-%d-%d", time.Now().UnixMilli(), n)
}

func newPairCode() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	enc = strings.ToUpper(enc)
	if len(enc) < 10 {
		return "", fmt.Errorf("pair code generation failed")
	}
	return enc[:5] + "-" + enc[5:10], nil
}

func randomToken(byteLen int) (string, error) {
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func normalizePairCode(code string) string {
	c := strings.ToUpper(strings.TrimSpace(code))
	c = strings.ReplaceAll(c, "-", "")
	c = strings.ReplaceAll(c, " ", "")
	return c
}

func parseAllowedOrigins(raw string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return out, nil
	}
	parts := strings.Split(trimmed, ",")
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item == "" {
			continue
		}
		if !strings.Contains(item, "://") {
			item = "https://" + item
		}
		u, err := url.Parse(item)
		if err != nil {
			return nil, fmt.Errorf("invalid AGENT_ALLOWED_ORIGINS item %q: %w", part, err)
		}
		host := strings.ToLower(strings.TrimSpace(u.Host))
		if host == "" {
			return nil, fmt.Errorf("invalid AGENT_ALLOWED_ORIGINS item %q: missing host", part)
		}
		out[host] = struct{}{}
	}
	return out, nil
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func getenv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func mustGetwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}

func parseListenPort(addr string) string {
	if strings.HasPrefix(addr, ":") {
		p := strings.TrimPrefix(addr, ":")
		if p != "" {
			return p
		}
		return "8088"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil || strings.TrimSpace(port) == "" {
		return "8088"
	}
	return port
}

func detectLocalHost() string {
	if ip := detectRouteIPv4(); ip != "" {
		return ip
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue
			}
			return ip.String()
		}
	}
	return "127.0.0.1"
}

func detectRouteIPv4() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr == nil || addr.IP == nil {
		return ""
	}
	ip := addr.IP.To4()
	if ip == nil || ip.IsLoopback() {
		return ""
	}
	return ip.String()
}

func isLoopbackHost(host string) bool {
	h := strings.TrimSpace(strings.ToLower(host))
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	if h == "" || h == "localhost" || h == "0.0.0.0" || h == "::" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
}

func expandHomePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot resolve home dir: %w", err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	if filepath.IsAbs(path) {
		return path, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return abs, nil
}

func mustHostname() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "desktop-agent"
	}
	return host
}
