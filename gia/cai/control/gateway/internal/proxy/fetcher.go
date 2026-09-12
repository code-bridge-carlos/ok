package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/1000carlospena-prog/carlos-gateway/pkg/types"
)

const PROXY_LIST_URL = "https://raw.githubusercontent.com/proxmint/free-proxy-list/main/proxies/all.txt"

type Fetcher struct {
	client  *http.Client
	manager *types.ProxyManager
}

func NewFetcher(manager *types.ProxyManager) *Fetcher {
	return &Fetcher{
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		manager: manager,
	}
}

func (f *Fetcher) FetchAndValidate(ctx context.Context) ([]*types.Proxy, error) {
	proxies, err := f.downloadProxyList(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to download proxy list: %w", err)
	}

	if len(proxies) == 0 {
		return nil, fmt.Errorf("no HTTP/HTTPS proxies found")
	}

	// Take last 100 (most recent)
	if len(proxies) > 100 {
		proxies = proxies[len(proxies)-100:]
	}

	validProxies := f.validateProxies(ctx, proxies)
	log.Printf("Proxy validation: %d candidates → %d valid", len(proxies), len(validProxies))
	return validProxies, nil
}

func (f *Fetcher) downloadProxyList(ctx context.Context) ([]*types.Proxy, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", PROXY_LIST_URL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP error: %d", resp.StatusCode)
	}

	var proxies []*types.Proxy
	scanner := bufio.NewScanner(resp.Body)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Accept HTTP and HTTPS proxies (Go handles CONNECT tunnel automatically)
		proxy, err := parseProxyLine(line)
		if err == nil {
			proxies = append(proxies, proxy)
		}
	}

	return proxies, scanner.Err()
}

func parseProxyLine(line string) (*types.Proxy, error) {
	parsed, err := url.Parse(line)
	if err != nil {
		return nil, err
	}

	host, port := parsed.Hostname(), parsed.Port()
	if host == "" || port == "" {
		return nil, fmt.Errorf("invalid host:port")
	}

	protocol := types.HTTP
	if parsed.Scheme == "https" {
		protocol = types.HTTPS
	} else if parsed.Scheme != "http" {
		return nil, fmt.Errorf("unsupported protocol: %s", parsed.Scheme)
	}

	return &types.Proxy{
		ID:       fmt.Sprintf("%s-%s", host, port),
		URL:      line,
		Protocol: protocol,
		Host:     host,
		Port:     port,
		AddedAt:  time.Now(),
	}, nil
}

func (f *Fetcher) validateProxies(ctx context.Context, proxies []*types.Proxy) []*types.Proxy {
	var valid []*types.Proxy
	var mu sync.Mutex
	var wg sync.WaitGroup

	semaphore := make(chan struct{}, 10)

	for _, p := range proxies {
		wg.Add(1)
		go func(proxy *types.Proxy) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			if f.validateProxy(ctx, proxy) {
				mu.Lock()
				valid = append(valid, proxy)
				mu.Unlock()
			}
		}(p)
	}

	wg.Wait()
	return valid
}

func (f *Fetcher) validateProxy(ctx context.Context, proxy *types.Proxy) bool {
	testURL := "http://httpbin.org/ip"

	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		return false
	}

	// Go handles CONNECT tunnel automatically for HTTPS targets
	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
	}
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: transport,
	}

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, "GET", testURL, nil)
	if err != nil {
		return false
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	body, _ := io.ReadAll(resp.Body)
	var ipResp struct {
		Origin string `json:"origin"`
	}
	json.Unmarshal(body, &ipResp)

	proxy.Latency = time.Since(start)
	proxy.LastTest = time.Now()
	proxy.IsActive = true

	if ipResp.Origin != "" {
		proxy.IPType = detectIPType(ipResp.Origin)
	} else {
		proxy.IPType = "unknown"
	}

	return true
}

func detectIPType(ipStr string) string {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return "unknown"
	}

	datacenterRanges := []string{
		"3.0.0.0/8", "34.0.0.0/8", "35.0.0.0/8", "52.0.0.0/8", "54.0.0.0/8", "18.0.0.0/8",
		"8.8.8.0/24", "34.64.0.0/10", "34.102.0.0/16", "34.120.0.0/14",
		"13.64.0.0/11", "40.64.0.0/10", "52.96.0.0/12", "104.40.0.0/13",
		"104.131.0.0/16", "159.65.0.0/16", "165.227.0.0/16", "68.183.0.0/16",
		"50.116.0.0/16", "139.162.0.0/16", "172.104.0.0/16",
		"5.196.0.0/16", "37.59.0.0/16",
		"104.238.0.0/16", "45.32.0.0/16",
	}

	for _, cidr := range datacenterRanges {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return "datacenter"
		}
	}

	return "residential"
}
