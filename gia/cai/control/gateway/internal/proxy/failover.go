package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/1000carlospena-prog/carlos-gateway/pkg/types"
)

type FailoverHandler struct {
	manager *types.ProxyManager
	logger  *Logger
}

func NewFailoverHandler(manager *types.ProxyManager) *FailoverHandler {
	return &FailoverHandler{
		manager: manager,
	}
}

func (f *FailoverHandler) DoWithFailover(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(req.Context(), 120*time.Second)
	defer cancel()

	f.manager.Mu.RLock()
	currentProxy := f.manager.ActiveProxy
	f.manager.Mu.RUnlock()

	if currentProxy == nil {
		return nil, fmt.Errorf("no active proxy available")
	}

	resp, err := f.doRequestWithProxy(ctx, req, currentProxy)
	if err == nil {
		return resp, nil
	}

	// Mark proxy as failed
	f.manager.Mu.Lock()
	currentProxy.FailCount++
	if currentProxy.FailCount >= 5 {
		currentProxy.IsActive = false
	}
	f.manager.Mu.Unlock()

	// Failover: try all available proxies
	f.manager.Mu.RLock()
	allProxies := f.manager.Proxies
	f.manager.Mu.RUnlock()

	for _, proxy := range allProxies {
		if proxy.URL == currentProxy.URL || !proxy.IsActive {
			continue
		}

		resp, err := f.doRequestWithProxy(ctx, req, proxy)
		if err == nil {
			f.manager.Mu.Lock()
			f.manager.ActiveProxy = proxy
			f.manager.Status.ActiveProxy = proxy
			f.manager.Status.LastFailover = time.Now()
			f.manager.Status.FailoverCount++
			f.manager.Mu.Unlock()
			return resp, nil
		}
	}

	return nil, fmt.Errorf("all proxies failed after %d attempts", len(allProxies))
}

func (f *FailoverHandler) doRequestWithProxy(ctx context.Context, req *http.Request, proxy *types.Proxy) (*http.Response, error) {
	newReq := req.Clone(ctx)

	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		return nil, err
	}

	// HTTPS proxy: Go uses CONNECT tunnel automatically for https:// targets
	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
	}

	client := &http.Client{
		Transport: transport,
	}

	return client.Do(newReq)
}
