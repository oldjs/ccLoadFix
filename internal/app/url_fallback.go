package app

// orderURLsWithSelector 返回用于故障切换的URL尝试顺序。
// 当 selector 可用且存在多个URL时：
// 1. 优先使用模型亲和性URL（上次成功的URL）
// 2. 其次用成功率+延迟加权随机选首跳
// 3. 其余URL按综合得分排序兜底
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

	sortedURLs := selector.SortURLs(channelID, urls)
	if len(sortedURLs) <= 1 {
		return sortedURLs
	}

	// 用带模型亲和性的选择（亲和URL会被优先返回）
	preferredURL, _ := selector.SelectURLForModel(channelID, model, urls)
	for i, entry := range sortedURLs {
		if entry.url != preferredURL {
			continue
		}
		if i == 0 {
			return sortedURLs
		}

		// 把首选URL提到最前面
		reordered := make([]sortedURL, 0, len(sortedURLs))
		reordered = append(reordered, entry)
		reordered = append(reordered, sortedURLs[:i]...)
		reordered = append(reordered, sortedURLs[i+1:]...)
		return reordered
	}

	return sortedURLs
}
