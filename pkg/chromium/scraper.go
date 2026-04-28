package chromium

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"downlink/pkg/models"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

// ScrapeResult contains the raw DOM and completion state from the extension.
type ScrapeResult struct {
	HTML  string
	State string // "loaded" | "failed"
}

// Scraper is the common interface for ChromiumScraper and ChromiumPool.
type Scraper interface {
	Scrape(ctx context.Context, rawURL string, triggers models.HostTriggers) (ScrapeResult, error)
	Connected() bool
}

// ChromiumScraper launches a single persistent Chromium browser loaded with
// the local extension. Each call to Scrape opens a new background tab,
// waits for the extension to POST the full DOM, and closes the tab.
// Request correlation is guaranteed via a per-request UUID sent through both
// the WebSocket command and the X-Downlink-Request-Id HTTP header.
type ChromiumScraper struct {
	extDir    string
	index     int  // used to derive a unique user-data-dir suffix
	noSandbox bool // pass --no-sandbox to Chromium (inverse of --use-sandbox)

	// Persistent browser state
	port        int
	userDataDir string
	httpServer  *http.Server
	cmd         *exec.Cmd

	// Single WebSocket connection from the extension background service worker.
	// All writes must be done under wsMu.
	wsMu    sync.Mutex
	wsConn  *websocket.Conn
	wsReady chan struct{} // closed once on first WebSocket connect
	wsOnce  sync.Once

	// In-flight scraping requests: requestId → result channel.
	pendingMu       sync.RWMutex
	pendingRequests map[string]chan scrapeResult

	// In-flight deduplication: concurrent Scrape calls for the same URL share one result.
	sf singleflight.Group

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc
}

type scrapeResult struct {
	html  string
	state string // "loaded" or "failed"
}

func NewChromiumScraper(extDir string, index int, noSandbox bool) *ChromiumScraper {
	ctx, cancel := context.WithCancel(context.Background())
	return &ChromiumScraper{
		extDir:          extDir,
		index:           index,
		noSandbox:       noSandbox,
		pendingRequests: make(map[string]chan scrapeResult),
		wsReady:         make(chan struct{}),
		ctx:             ctx,
		cancel:          cancel,
	}
}

// Start launches the persistent Chromium process and the shared HTTP server.
// It must be called once before any Scrape calls.
func (s *ChromiumScraper) Start() error {
	// 1. Allocate a free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("chromium scraper: failed to get free port: %w", err)
	}
	s.port = ln.Addr().(*net.TCPAddr).Port

	// 2. Create a persistent user-data-dir so cookies/sessions survive across scrapes.
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		ln.Close()
		return fmt.Errorf("chromium scraper: failed to get cache dir: %w", err)
	}

	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		ln.Close()
		return fmt.Errorf("chromium scraper: failed to create user-data-dir: %w", err)
	}

	s.userDataDir, err = os.MkdirTemp(cacheDir, "downlink-chromium-*")
	if err != nil {
		ln.Close()
		return fmt.Errorf("chromium scraper: failed to generate temp dir name: %w", err)
	}

	// 3. Write runtime-config.json with only the port; triggers are sent dynamically.
	rtCfgData, err := json.Marshal(map[string]any{"port": s.port})
	if err != nil {
		ln.Close()
		return fmt.Errorf("chromium scraper: failed to marshal runtime config: %w", err)
	}
	rtCfgPath := filepath.Join(s.extDir, "runtime-config.json")
	if err := os.WriteFile(rtCfgPath, rtCfgData, 0644); err != nil {
		ln.Close()
		return fmt.Errorf("chromium scraper: failed to write runtime config: %w", err)
	}

	// 4. Build and start the HTTP server.
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.wsHandler)
	mux.HandleFunc("/dom", s.domHandler)
	s.httpServer = &http.Server{Handler: mux}
	go func() {
		if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.WithError(err).Error("chromium scraper: HTTP server error")
		}
	}()

	// 5. Resolve extension directory and launch Chromium.
	absExtDir, err := filepath.Abs(s.extDir)
	if err != nil {
		s.httpServer.Close()
		return fmt.Errorf("chromium scraper: failed to resolve extension dir: %w", err)
	}

	args := []string{
		"--disable-dev-shm-usage",
		"--disable-extensions-except=" + absExtDir,
		"--load-extension=" + absExtDir,
		"--user-data-dir=" + s.userDataDir,
		"--no-first-run",
		"--no-default-browser-check",
	}
	if s.noSandbox {
		args = append([]string{"--no-sandbox"}, args...)
	}

	s.cmd = exec.Command("chromium", args...)

	log.WithFields(log.Fields{
		"port":        s.port,
		"extDir":      absExtDir,
		"userDataDir": s.userDataDir,
	}).Info("chromium scraper: launching browser")

	if err := s.cmd.Start(); err != nil {
		s.httpServer.Close()
		return fmt.Errorf("chromium scraper: failed to start chromium: %w", err)
	}

	// 6. Start background ping loop to keep the WebSocket alive.
	go s.pingLoop()

	// 7. Wait for the extension service worker to connect via WebSocket before
	// returning, so that the first Scrape() call never races the handshake.
	log.Info("chromium scraper: waiting for extension WebSocket connection...")
	select {
	case <-s.wsReady:
		log.Info("chromium scraper: extension ready")
	case <-time.After(30 * time.Second):
		s.Stop()
		return fmt.Errorf("chromium scraper: timed out waiting for extension WebSocket connection")
	}

	return nil
}

// Stop shuts down the Chromium process and the HTTP server gracefully.
func (s *ChromiumScraper) Stop() {
	s.cancel()

	s.wsMu.Lock()
	if s.wsConn != nil {
		s.wsConn.Close()
		s.wsConn = nil
	}
	s.wsMu.Unlock()

	if s.httpServer != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		s.httpServer.Shutdown(shutdownCtx)
	}

	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
		s.cmd.Wait()
	}

	log.Info("chromium scraper: stopped")
}

// Connected returns true if the extension WebSocket is currently established.
func (s *ChromiumScraper) Connected() bool {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	return s.wsConn != nil
}

// wsHandler upgrades the connection to WebSocket and holds it as the single
// communication channel to the extension background service worker.
func (s *ChromiumScraper) wsHandler(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.WithError(err).Error("chromium scraper: WebSocket upgrade failed")
		return
	}

	// Evict any stale connection before storing the new one.
	s.wsMu.Lock()
	if s.wsConn != nil {
		s.wsConn.Close()
	}
	s.wsConn = conn
	s.wsMu.Unlock()

	// Unblock Start() on the first connection.
	s.wsOnce.Do(func() { close(s.wsReady) })

	log.WithField("remoteAddr", conn.RemoteAddr()).Info("chromium scraper: WebSocket connected")

	// Gorilla requires a read loop running to process control frames (pongs, close).
	go func() {
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				s.wsMu.Lock()
				if s.wsConn == conn {
					s.wsConn = nil
				}
				s.wsMu.Unlock()
				log.WithError(err).Info("chromium scraper: WebSocket disconnected")
				return
			}
		}
	}()
}

// domHandler receives the full DOM from the extension via HTTP POST and routes it
// to the correct in-flight Scrape call using the X-Downlink-Request-Id header.
func (s *ChromiumScraper) domHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	requestId := r.Header.Get("X-Downlink-Request-Id")
	if requestId == "" {
		http.Error(w, "missing X-Downlink-Request-Id", http.StatusBadRequest)
		return
	}

	state := r.Header.Get("X-Downlink-State")
	if state == "" {
		state = "loaded"
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusInternalServerError)
		return
	}

	// Respond immediately before doing the channel send.
	w.WriteHeader(http.StatusOK)

	s.pendingMu.RLock()
	ch, ok := s.pendingRequests[requestId]
	s.pendingMu.RUnlock()

	if !ok {
		log.WithField("requestId", requestId).Warn("chromium scraper: received DOM for unknown requestId")
		return
	}

	select {
	case ch <- scrapeResult{html: string(body), state: state}:
	default:
		log.WithField("requestId", requestId).Warn("chromium scraper: result channel full, dropping DOM")
	}
}

// pingLoop sends periodic pings to keep the WebSocket connection alive and
// prevent the extension service worker from going idle.
func (s *ChromiumScraper) pingLoop() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.wsMu.Lock()
			conn := s.wsConn
			s.wsMu.Unlock()
			if conn == nil {
				continue
			}
			data, _ := json.Marshal(map[string]string{"type": "ping"})
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				log.WithError(err).Debug("chromium scraper: ping write error")
				continue
			}
			conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
		}
	}
}

// Scrape opens a new browser tab for the given URL, waits for the extension
// to extract and POST the DOM, and returns the raw HTML and state.
// The caller's ctx deadline is honoured alongside the internal 30-second timeout.
// Concurrent calls for the same URL are deduplicated: only one tab is opened
// and all callers share the result.
func (s *ChromiumScraper) Scrape(ctx context.Context, rawURL string, triggers models.HostTriggers) (ScrapeResult, error) {
	v, err, _ := s.sf.Do(rawURL, func() (any, error) {
		return s.scrape(ctx, rawURL, triggers)
	})
	if err != nil {
		return ScrapeResult{}, err
	}
	return v.(ScrapeResult), nil
}

func (s *ChromiumScraper) scrape(ctx context.Context, rawURL string, triggers models.HostTriggers) (ScrapeResult, error) {
	requestId := uuid.New().String()
	ch := make(chan scrapeResult, 1)

	s.pendingMu.Lock()
	s.pendingRequests[requestId] = ch
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pendingRequests, requestId)
		s.pendingMu.Unlock()
	}()

	// Send the scrape command to the extension over WebSocket.
	cmd := map[string]any{
		"type":      "scrape",
		"requestId": requestId,
		"url":       rawURL,
		"triggers":  triggers,
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		return ScrapeResult{}, fmt.Errorf("chromium scraper: failed to marshal scrape command: %w", err)
	}

	s.wsMu.Lock()
	conn := s.wsConn
	s.wsMu.Unlock()
	if conn == nil {
		return ScrapeResult{}, fmt.Errorf("chromium scraper: no WebSocket connection — extension not ready")
	}

	s.wsMu.Lock()
	writeErr := conn.WriteMessage(websocket.TextMessage, data)
	s.wsMu.Unlock()
	if writeErr != nil {
		return ScrapeResult{}, fmt.Errorf("chromium scraper: failed to send scrape command: %w", writeErr)
	}

	log.WithFields(log.Fields{
		"url":       rawURL,
		"requestId": requestId,
	}).Info("chromium scraper: scrape command sent")

	// Wait for DOM, timeout, or shutdown.
	select {
	case result := <-ch:
		log.WithFields(log.Fields{
			"url":       rawURL,
			"requestId": requestId,
			"state":     result.state,
		}).Info("chromium scraper: DOM received")
		return ScrapeResult{HTML: result.html, State: result.state}, nil
	case <-time.After(30 * time.Second):
		return ScrapeResult{}, fmt.Errorf("chromium scraper: timed out waiting for DOM from %s", rawURL)
	case <-ctx.Done():
		return ScrapeResult{}, fmt.Errorf("chromium scraper: context cancelled for %s: %w", rawURL, ctx.Err())
	case <-s.ctx.Done():
		return ScrapeResult{}, fmt.Errorf("chromium scraper: scraper shut down during request for %s", rawURL)
	}
}
