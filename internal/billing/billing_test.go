package billing

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api-prime/internal/types"
)

// TestRecordQueryLoop 验证 Record→Query 闭环：写入多条记录后按 key 聚合。
func TestRecordQueryLoop(t *testing.T) {
	l, err := NewLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	records := []types.UsageRecord{
		{Time: now.Add(-2 * time.Hour), APIKeyID: "keyA", AccountID: "a1", Model: "m1", PromptTok: 100, CompTok: 50, Credit: 1.5, LatencyMs: 120, OK: true},
		{Time: now.Add(-1 * time.Hour), APIKeyID: "keyA", AccountID: "a1", Model: "m1", PromptTok: 200, CompTok: 100, Credit: 3.0, LatencyMs: 200, OK: true},
		{Time: now, APIKeyID: "keyB", AccountID: "a2", Model: "m2", PromptTok: 10, CompTok: 5, Credit: 0.2, LatencyMs: 50, OK: false},
	}
	for _, r := range records {
		if err := l.Record(r); err != nil {
			t.Fatal(err)
		}
	}

	s, err := l.Query("keyA", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Requests != 2 || s.PromptTokens != 300 || s.CompletionTokens != 150 || s.TotalCredit != 4.5 {
		t.Fatalf("keyA 聚合错误: %+v", s)
	}

	// 全 key 查询应包含失败请求（不丢样本）
	all, err := l.Query("", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if all.Requests != 3 || all.TotalCredit != 4.7 {
		t.Fatalf("全量聚合错误: %+v", all)
	}

	// 时间过滤：只取最近 30 分钟，应只剩 keyB 那一条
	recent, err := l.Query("", now.Add(-30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if recent.Requests != 1 || recent.PromptTokens != 10 {
		t.Fatalf("时间过滤错误: %+v", recent)
	}
}

// TestQueryEmptyLedger 空账本查询返回零值不报错。
func TestQueryEmptyLedger(t *testing.T) {
	l, err := NewLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := l.Query("any", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if s != (Summary{}) {
		t.Fatalf("空账本应返回零值: %+v", s)
	}
}

// TestQuerySkipsBadLines 脏行不影响查询。
func TestQuerySkipsBadLines(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Record(types.UsageRecord{Time: time.Now(), APIKeyID: "k", PromptTok: 7, Credit: 0.1, OK: true}); err != nil {
		t.Fatal(err)
	}
	// 手工追加一行坏数据
	f, err := os.OpenFile(filepath.Join(dir, "usage", ledgerFileName), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("这不是 JSON\n")
	f.Close()

	s, err := l.Query("k", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Requests != 1 || s.PromptTokens != 7 {
		t.Fatalf("坏行应被跳过: %+v", s)
	}
}

// TestLedgerFilePerm 账本文件权限为 0600。
func TestLedgerFilePerm(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Record(types.UsageRecord{Time: time.Now(), APIKeyID: "k", OK: true}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "usage", ledgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("账本文件权限应为 0600，实际 %o", fi.Mode().Perm())
	}
}
