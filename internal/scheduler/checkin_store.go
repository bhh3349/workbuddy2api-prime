package scheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"workbuddy2api-prime/internal/types"
)

// 签到历史保留策略（参考 HanawaBanana 的 checkin_history.go）：
// 滚动保留最近 MaxKeep 条，且不超过 MaxAge 天。
const (
	checkinMaxKeep = 200
	checkinMaxAge  = 7 * 24 * time.Hour
	checkinFile    = "checkin.json"
	checkinVersion = 1
)

// checkinFileData 是 checkin.json 的落盘格式（带版本号，便于未来迁移）。
type checkinFileData struct {
	Version int                   `json:"version"`
	Records []types.CheckinResult `json:"records"`
}

// CheckinStore 负责签到结果的原子落盘与只读查询。
// 设计动机（HanawaBanana）：此前签到结果只存在于 HTTP 响应里，进程不缓存、
// 磁盘无文件，"刷新页面即失忆"。
type CheckinStore struct {
	mu      sync.Mutex
	path    string
	records []types.CheckinResult // 按时间正序（旧→新）
}

// NewCheckinStore 创建存储；dataDir 不存在则创建（0700），
// 已有 checkin.json 则加载（损坏时记日志并从空开始，不阻断启动）。
func NewCheckinStore(dataDir string) (*CheckinStore, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	cs := &CheckinStore{path: filepath.Join(dataDir, checkinFile)}
	data, err := os.ReadFile(cs.path)
	if err != nil {
		if os.IsNotExist(err) {
			return cs, nil
		}
		return nil, err
	}
	var fd checkinFileData
	if err := json.Unmarshal(data, &fd); err != nil {
		// 文件损坏：放弃历史，保证服务可用
		return cs, nil
	}
	cs.records = fd.Records
	cs.pruneLocked(time.Now())
	return cs, nil
}

// Append 追加一条签到结果并原子落盘（tmp + rename，0600）。
func (cs *CheckinStore) Append(r types.CheckinResult) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.records = append(cs.records, r)
	cs.pruneLocked(time.Now())
	return cs.flushLocked()
}

// History 返回签到历史（按时间倒序：新→旧）。
// 只读，不触发任何上游请求（HanawaBanana 的 GET /v1/checkin/history 设计）。
func (cs *CheckinStore) History() []types.CheckinResult {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]types.CheckinResult, len(cs.records))
	for i, r := range cs.records {
		out[len(cs.records)-1-i] = r
	}
	return out
}

// pruneLocked 按保留策略裁剪（调用方需持有锁）。
func (cs *CheckinStore) pruneLocked(now time.Time) {
	cutoff := now.Add(-checkinMaxAge)
	kept := cs.records[:0]
	for _, r := range cs.records {
		if !r.Time.Before(cutoff) {
			kept = append(kept, r)
		}
	}
	cs.records = kept
	if len(cs.records) > checkinMaxKeep {
		cs.records = append([]types.CheckinResult(nil), cs.records[len(cs.records)-checkinMaxKeep:]...)
	}
	// 保持正序
	sort.Slice(cs.records, func(i, j int) bool { return cs.records[i].Time.Before(cs.records[j].Time) })
}

// flushLocked 原子写盘（调用方需持有锁）。
func (cs *CheckinStore) flushLocked() error {
	fd := checkinFileData{Version: checkinVersion, Records: cs.records}
	data, err := json.MarshalIndent(fd, "", "  ")
	if err != nil {
		return err
	}
	tmp := cs.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, cs.path)
}
