package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/1000carlospena-prog/carlos-gateway/pkg/types"
)

type Manager struct {
	*types.ProxyManager
	fetcher     *Fetcher
	scheduler   *Scheduler
	failover    *FailoverHandler
	logger      *Logger
	logs        []string
	logMutex    sync.Mutex
}

func NewManager(config *types.ProxyManagerConfig) (*Manager, error) {
	if config == nil {
		config = &types.ProxyManagerConfig{
			ProxyListURL:     "https://raw.githubusercontent.com/proxmint/free-proxy-list/main/proxies/all.txt",
			RefreshInterval:  time.Hour,
			MaxProxiesPerHour: 100,
			TestTimeout:      10 * time.Second,
			RequestTimeout:   10 * time.Second,
			MaxFailures:      5,
			FailoverRetries:  100,
			FailoverTimeout:  10 * time.Second,
		}
	}

	m := &Manager{
		ProxyManager: &types.ProxyManager{
			Config: config,
			Status: &types.ProxyStatus{
				IsHealthy: true,
			},
		},
		logs: make([]string, 0, 1000),
	}

	m.fetcher = NewFetcher(m.ProxyManager)
	m.scheduler = NewScheduler(m.ProxyManager, m.fetcher)
	m.failover = NewFailoverHandler(m.ProxyManager)

	return m, nil
}

func (m *Manager) Start() error {
	m.scheduler.Start()
	// Initial fetch
	go m.scheduledRefresh()
	return nil
}

func (m *Manager) Stop() {
	m.scheduler.Stop()
}

func (m *Manager) scheduledRefresh() {
	m.scheduler.scheduledRefresh()
}

func (m *Manager) GetActiveProxy() *types.Proxy {
	m.Mu.RLock()
	defer m.Mu.RUnlock()
	return m.ActiveProxy
}

func (m *Manager) GetProxies() []*types.Proxy {
	m.Mu.RLock()
	defer m.Mu.RUnlock()
	return m.Proxies
}

func (m *Manager) GetStatus() *types.ProxyStatus {
	m.Mu.RLock()
	defer m.Mu.RUnlock()
	return m.Status
}

func (m *Manager) RefreshProxies() error {
	return m.scheduler.RefreshNow()
}

func (m *Manager) ChangeProxy() error {
	return m.scheduler.ChangeProxy()
}

func (m *Manager) DoRequest(req *http.Request) (*http.Response, error) {
	return m.failover.DoWithFailover(req)
}

func (m *Manager) GetLogs() []string {
	m.logMutex.Lock()
	defer m.logMutex.Unlock()
	return m.logs
}

func (m *Manager) addLog(msg string) {
	m.logMutex.Lock()
	defer m.logMutex.Unlock()
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	logEntry := fmt.Sprintf("[%s] %s", timestamp, msg)
	m.logs = append(m.logs, logEntry)
	if len(m.logs) > 1000 {
		m.logs = m.logs[len(m.logs)-1000:]
	}
}

// HTTP Handlers for UI
func (m *Manager) StatusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	response := &types.ProxyListResponse{
		Proxies:     m.GetProxies(),
		ActiveProxy: m.GetActiveProxy(),
		Status:      m.GetStatus(),
		Logs:        m.GetLogs(),
	}
	json.NewEncoder(w).Encode(response)
}

func (m *Manager) RefreshHandler(w http.ResponseWriter, r *http.Request) {
	err := m.RefreshProxies()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "refreshed"})
}

func (m *Manager) ChangeHandler(w http.ResponseWriter, r *http.Request) {
	err := m.ChangeProxy()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "changed"})
}

func (m *Manager) ClearCacheHandler(w http.ResponseWriter, r *http.Request) {
	m.Mu.Lock()
	m.Status.FailoverCount = 0
	m.Mu.Unlock()

	m.addLog("Cache cleared")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "cache cleared"})
}

func (m *Manager) ListHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	proxies := m.GetProxies()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"proxies": proxies,
		"total":   len(proxies),
	})
}

func (m *Manager) UIHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	http.ServeFile(w, r, "web/static/proxy-ui.html")
}

func (m *Manager) LogsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	logs := m.GetLogs()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"logs":   logs,
		"count":  len(logs),
		"format": "text",
	})
}