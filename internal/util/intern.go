// Package util 提供通用工具函数
package util

import "sync"

// stringInternPool 字符串驻留池
// 用于高频低基数字符串（model name、channel type、base URL 等），
// 让相同值在整个进程中只保留一份堆分配，减少 GC 扫描开销
var stringInternPool = struct {
	sync.RWMutex
	m map[string]string
}{m: make(map[string]string, 256)}

// InternString 返回 s 的驻留版本
// 如果池中已有相同值，返回池中的引用（零分配）
// 否则存入池中并返回
func InternString(s string) string {
	if s == "" {
		return ""
	}
	stringInternPool.RLock()
	if interned, ok := stringInternPool.m[s]; ok {
		stringInternPool.RUnlock()
		return interned
	}
	stringInternPool.RUnlock()

	stringInternPool.Lock()
	// 双重检查
	if interned, ok := stringInternPool.m[s]; ok {
		stringInternPool.Unlock()
		return interned
	}
	stringInternPool.m[s] = s
	stringInternPool.Unlock()
	return s
}
