package types

import (
	"context"
	"net/http"
	"sync"
	"time"
)

type ProxyProtocol string

const (
	HTTP  ProxyProtocol = "http"
	HTTPS ProxyProtocol = "https"
	SOCKS5 ProxyProtocol = "socks5"
)

type Proxy struct {
	ID         string        `json:"id"`
	URL        string        `json:"url"`
	Protocol   ProxyProtocol `json:"protocol"`
	Host       string        `json:"host"`
	Port       string        `json:"port"`
	Country    string        `json:"country"`
	IPType     string        `json:"ip_type"` // "datacenter" or "residential"
	Latency    time.Duration `json:"latency"`
	LastTest   time.Time     `json:"last_test"`
	IsActive   bool          `json:"is_active"`
	FailCount  int           `json:"fail_count"`
	LastUsed   time.Time     `json:"last_used"`
	AddedAt    time.Time     `json:"added_at"`
}

type ProxyManagerConfig struct {
	ProxyListURL       string        `json:"proxy_list_url"`
	RefreshInterval    time.Duration `json:"refresh_interval"`
	MaxProxiesPerHour  int           `json:"max_proxies_per_hour"`
	TestTimeout        time.Duration `json:"test_timeout"`
	RequestTimeout     time.Duration `json:"request_timeout"`
	MaxFailures        int           `json:"max_failures"`
	FailoverRetries    int           `json:"failover_retries"`
	FailoverTimeout    time.Duration `json:"failover_timeout"`
}

type ProxyManager struct {
	Mu           sync.RWMutex
	Proxies      []*Proxy
	ActiveProxy  *Proxy
	Config       *ProxyManagerConfig
	HttpClient   *http.Client
	Scheduler    *Scheduler
	FailoverChan chan *FailoverRequest
	Status       *ProxyStatus
}

type ProxyStatus struct {
	ActiveProxy    *Proxy    `json:"active_proxy"`
	TotalProxies   int       `json:"total_proxies"`
	HealthyCount   int       `json:"healthy_count"`
	LastRefresh    time.Time `json:"last_refresh"`
	NextRefresh    time.Time `json:"next_refresh"`
	FailoverCount  int       `json:"failover_count"`
	LastFailover   time.Time `json:"last_failover"`
	IsHealthy      bool      `json:"is_healthy"`
}

type FailoverRequest struct {
	OriginalRequest *http.Request
	ResponseChan    chan *FailoverResponse
	Attempts        int
}

type FailoverResponse struct {
	Response   *http.Response
	Error      error
	ProxyUsed  *Proxy
}

type ProxyListResponse struct {
	Proxies      []*Proxy     `json:"proxies"`
	ActiveProxy  *Proxy       `json:"active_proxy"`
	Status       *ProxyStatus `json:"status"`
	Logs         []string     `json:"logs"`
}

type Scheduler struct {
	manager     *ProxyManager
	fetcher     *Fetcher
	ticker      *time.Ticker
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	nextRefresh time.Time
}

type Fetcher struct {
	client  *http.Client
	manager *ProxyManager
}

type FailoverHandler struct {
	manager *ProxyManager
	client  *http.Client
}