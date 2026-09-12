package proxy

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/1000carlospena-prog/carlos-gateway/pkg/types"
)

type Scheduler struct {
	manager     *types.ProxyManager
	fetcher     *Fetcher
	ticker      *time.Ticker
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	nextRefresh time.Time
}

func NewScheduler(manager *types.ProxyManager, fetcher *Fetcher) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		manager: manager,
		fetcher: fetcher,
		ctx:     ctx,
		cancel:  cancel,
	}
}

func (s *Scheduler) Start() {
	// Initial refresh
	go s.scheduledRefresh()

	// Schedule hourly at minute 0
	go s.scheduleHourlyRefresh()
}

func (s *Scheduler) scheduleHourlyRefresh() {
	for {
		now := time.Now()
		// Next hour at minute 0
		next := time.Date(now.Year(), now.Month(), now.Day(), now.Hour()+1, 0, 0, 0, now.Location())
		duration := next.Sub(now)

		select {
		case <-s.ctx.Done():
			return
		case <-time.After(duration):
			s.scheduledRefresh()
		}
	}
}

func (s *Scheduler) scheduledRefresh() {
	s.mu.Lock()
	defer s.mu.Unlock()

	proxies, err := s.fetcher.FetchAndValidate(s.ctx)
	if err != nil {
		// Log error but keep current proxies
		return
	}

	// Update manager
	s.manager.Mu.Lock()
	s.manager.Proxies = proxies
	s.manager.Status.LastRefresh = time.Now()
	s.manager.Status.NextRefresh = time.Now().Add(time.Hour)
	s.manager.Status.TotalProxies = len(proxies)

	// Randomly select one as active
	if len(proxies) > 0 {
		idx, _ := rand.Int(rand.Reader, big.NewInt(int64(len(proxies))))
		s.manager.ActiveProxy = proxies[idx.Int64()]
		s.manager.ActiveProxy.IsActive = true
		s.manager.Status.ActiveProxy = s.manager.ActiveProxy
	}
	s.manager.Mu.Unlock()
}

func (s *Scheduler) Stop() {
	s.cancel()
	if s.ticker != nil {
		s.ticker.Stop()
	}
}

func (s *Scheduler) RefreshNow() error {
	s.scheduledRefresh()
	return nil
}

func (s *Scheduler) ChangeProxy() error {
	s.manager.Mu.Lock()
	defer s.manager.Mu.Unlock()

	proxies := s.manager.Proxies
	if len(proxies) == 0 {
		return fmt.Errorf("no proxies available")
	}

	// Pick random proxy different from current active
	var newProxy *types.Proxy
	for attempts := 0; attempts < 100; attempts++ {
		idx, _ := rand.Int(rand.Reader, big.NewInt(int64(len(proxies))))
		candidate := proxies[idx.Int64()]
		if candidate.URL != s.manager.ActiveProxy.URL {
			newProxy = candidate
			break
		}
	}

	if newProxy == nil {
		// If all proxies are the same (shouldn't happen), pick first
		newProxy = proxies[0]
	}

	// Deactivate old
	if s.manager.ActiveProxy != nil {
		s.manager.ActiveProxy.IsActive = false
	}

	// Activate new
	newProxy.IsActive = true
	s.manager.ActiveProxy = newProxy
	s.manager.Status.ActiveProxy = newProxy
	s.manager.Status.LastFailover = time.Now()
	s.manager.Status.FailoverCount++

	return nil
}