package app

// orderURLsWithSelector 返回用于故障切换的 URL 尝试顺序。
// 这里一次性产出首跳和 fallback 计划，避免先选首跳再重排导致前后不一致。
func orderURLsWithSelector(selector *URLSelector, channelID int64, urls []string, model string) []sortedURL {
	if len(urls) == 0 {
		return nil
	}
	if len(urls) == 1 {
		return []sortedURL{{url: urls[0], idx: 0}}
	}
	if selector == nil {
		ordered := make([]sortedURL, len(urls))
		for i, u := range urls {
			ordered[i] = sortedURL{url: u, idx: i}
		}
		return ordered
	}
	return selector.planURLsForModel(channelID, model, urls)
}

// orderDiagnosticURLsWithSelector 返回给后台测试/诊断用的 URL 顺序。
// 先沿用统一 planner 的优先级，再把被 canary 隐藏掉的 URL 追加回来，保证人工诊断时能把所有 URL 试完。
func orderDiagnosticURLsWithSelector(selector *URLSelector, channelID int64, urls []string, model string) []sortedURL {
	planned := orderURLsWithSelector(selector, channelID, urls, model)
	if len(planned) >= len(urls) {
		return planned
	}

	seen := make(map[string]struct{}, len(planned))
	expanded := append([]sortedURL(nil), planned...)
	for _, entry := range planned {
		seen[entry.url] = struct{}{}
	}
	for i, rawURL := range urls {
		if _, ok := seen[rawURL]; ok {
			continue
		}
		expanded = append(expanded, sortedURL{url: rawURL, idx: i})
	}
	return expanded
}
