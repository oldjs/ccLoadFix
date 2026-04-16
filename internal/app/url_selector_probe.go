package app

import (
	"context"
	"errors"
	"log"
	"net"
	"net/url"
	"time"
)

// extractHostPort 从URL字符串提取 host:port，用于TCP连接测试。
// 如果URL中没有端口，根据scheme自动补全（https→443, http→80）。
func extractHostPort(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return ""
	}
	host := parsed.Hostname()
	if host == "" {
		return ""
	}
	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			return ""
		}
	}
	return net.JoinHostPort(host, port)
}

// ProbeURLs 对还没任何延迟数据的 URL 做并行 TCP 探测，记录 RTT 作为冷启动种子。
func (s *URLSelector) ProbeURLs(parentCtx context.Context, channelID int64, urls []string) {
	if len(urls) <= 1 {
		return
	}
	if parentCtx == nil {
		parentCtx = context.Background()
	}

	sh := s.getShard(channelID)

	// 原子筛选+占位，避免并发请求重复探测同一URL
	sh.mu.Lock()
	now := time.Now()
	s.maybeCleanupShard(sh, now)
	unknowns := make([]string, 0, len(urls))
	for _, u := range urls {
		key := urlKey{channelID: channelID, url: u}
		if _, known := sh.latencies[key]; known {
			continue
		}
		if _, known := sh.probeLatencies[key]; known {
			continue
		}
		if _, inFlight := sh.probing[key]; inFlight {
			continue
		}
		sh.probing[key] = now
		unknowns = append(unknowns, u)
	}
	sh.mu.Unlock()

	if len(unknowns) == 0 {
		return
	}

	probeTimeout := s.probeTimeout
	if probeTimeout <= 0 {
		probeTimeout = defaultURLSelectorProbeTimeout
	}

	ctx, cancel := context.WithTimeout(parentCtx, probeTimeout)
	defer cancel()

	type probeResult struct {
		url     string
		latency time.Duration
		err     error
	}

	results := make(chan probeResult, len(unknowns))
	pending := make(map[string]struct{}, len(unknowns))
	clearProbing := func(probedURL string) {
		key := urlKey{channelID: channelID, url: probedURL}
		sh.mu.Lock()
		delete(sh.probing, key)
		sh.mu.Unlock()
	}
	for _, u := range unknowns {
		pending[u] = struct{}{}
		go func(rawURL string) {
			host := extractHostPort(rawURL)
			if host == "" {
				results <- probeResult{url: rawURL, err: net.UnknownNetworkError("invalid URL")}
				return
			}

			start := time.Now()
			conn, err := s.probeDial(ctx, "tcp", host)
			if err != nil {
				results <- probeResult{url: rawURL, err: err}
				return
			}
			_ = conn.Close()
			results <- probeResult{url: rawURL, latency: time.Since(start)}
		}(u)
	}

	probed := 0
	failed := 0
	handleResult := func(r probeResult) {
		if _, ok := pending[r.url]; !ok {
			return
		}
		delete(pending, r.url)
		defer clearProbing(r.url)
		if r.err != nil {
			// 主动取消不算URL坏，只是这轮探测被打断了。
			if errors.Is(r.err, context.Canceled) {
				return
			}
			s.CooldownURL(channelID, r.url)
			failed++
			return
		}
		latency := r.latency
		if latency <= 0 {
			latency = time.Millisecond
		}
		s.RecordProbeLatency(channelID, r.url, latency)
		probed++
	}

	for range len(unknowns) {
		select {
		case r := <-results:
			handleResult(r)
		case <-ctx.Done():
			// 超时/取消：先吃掉已经回来的结果
			ctxErr := ctx.Err()
			shouldCooldownPending := errors.Is(ctxErr, context.DeadlineExceeded)
			for {
				select {
				case r := <-results:
					handleResult(r)
				default:
					for pendingURL := range pending {
						clearProbing(pendingURL)
						if shouldCooldownPending {
							s.CooldownURL(channelID, pendingURL)
							failed++
						}
					}
					log.Printf("[PROBE] TCP探测提前结束(%v)，已完成=%d/%d", ctxErr, probed+failed, len(unknowns))
					if probed > 0 || failed > 0 {
						log.Printf("[PROBE] 渠道ID=%d TCP探测完成: 成功=%d 失败=%d", channelID, probed, failed)
					}
					return
				}
			}
		}
	}

	if probed > 0 || failed > 0 {
		log.Printf("[PROBE] 渠道ID=%d TCP探测完成: 成功=%d 失败=%d", channelID, probed, failed)
	}
}
