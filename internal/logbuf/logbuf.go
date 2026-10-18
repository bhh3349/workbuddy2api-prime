// Package logbuf 提供进程内日志环形缓冲，供面板「运行日志」视图查询。
// cmd/server 启动时把 log 的输出同时 tee 到这里（log.SetOutput(io.MultiWriter(...))）。
package logbuf

import (
	"strings"
	"sync"
	"time"
)

// Entry 是单条日志。
type Entry struct {
	Time time.Time `json:"ts"`
	Chan string    `json:"ch"`   // task | chat | sys（按文本关键词粗分）
	Text string    `json:"text"`
}

const maxEntries = 2000

var (
	mu      sync.Mutex
	entries []Entry
)

// Write 实现 io.Writer：按行切分存入环形缓冲（供 log.SetOutput tee 用）。
func Write(p []byte) (int, error) {
	mu.Lock()
	defer mu.Unlock()
	for _, line := range strings.Split(string(p), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		entries = append(entries, Entry{Time: time.Now(), Chan: classify(line), Text: line})
		if len(entries) > maxEntries {
			entries = entries[len(entries)-maxEntries:]
		}
	}
	return len(p), nil
}

// classify 按关键词粗分频道。
func classify(line string) string {
	l := strings.ToLower(line)
	switch {
	case strings.Contains(l, "growth") || strings.Contains(l, "checkin") || strings.Contains(l, "keepalive") || strings.Contains(l, "task"):
		return "task"
	case strings.Contains(l, "chat") || strings.Contains(l, "completion") || strings.Contains(l, "upstream"):
		return "chat"
	default:
		return "sys"
	}
}

// Query 返回最近 limit 条（limit<=0 时默认 200，上限 1000），可按频道过滤。
func Query(limit int, ch string) []Entry {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	mu.Lock()
	defer mu.Unlock()
	out := make([]Entry, 0, limit)
	for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
		e := entries[i]
		if ch != "" && ch != "all" && e.Chan != ch {
			continue
		}
		out = append(out, e)
	}
	// 反转为时间正序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// writer 适配 io.Writer 接口。
type writer struct{}

func (writer) Write(p []byte) (int, error) { return Write(p) }

// Writer 返回可传给 io.MultiWriter 的 Writer。
func Writer() interface{ Write([]byte) (int, error) } { return writer{} }
