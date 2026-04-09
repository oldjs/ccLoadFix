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
// 设计目标：多URL渠道首次被选中时，避免随机选到网络延迟高的URL。
//
// TCP连接时间反映纯网络延迟（DNS+TCP握手），与模型推理时间无关，
// 因此不会误杀推理模型的长首字节等待。
//
// 探测结果仅作为初始EWMA种子，后续真实请求的TTFB会纳入EWMA并逐步校准。
func (s *URLSelector) ProbeURLs(parentCtx context.Context, channelID int64, urls []string) {
	if len(urls) <= 1 {
		return
	}
	if parentCtx == nil {
		parentCtx = context.Background()
	}

	// 原子筛选+占位，避免并发请求重复探测同一URL。
	s.mu.Lock()
	now := time.Now()
	s.maybeCleanupLocked(now)
	unknowns := make([]string, 0, len(urls))
	for _, u := range urls {
		key := urlKey{channelID: channelID, url: u}
		if _, known := s.latencies[key]; known {
			continue
		}
		if _, known := s.probeLatencies[key]; known {
			continue
		}
		if _, inFlight := s.probing[key]; inFlight {
			continue
		}
		s.probing[key] = now
		unknowns = append(unknowns, u)
	}
	s.mu.Unlock()

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
		s.mu.Lock()
		delete(s.probing, key)
		s.mu.Unlock()
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
			// 超时/取消：先吃掉已经回来的结果，再处理剩下没回来的URL。
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
