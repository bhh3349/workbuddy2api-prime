package apikeys

import (
	"strings"
	"testing"
	"time"
)

// 建库 + 签发 + 正常校验走通。
func TestCreateAndVerify(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plain, k, err := s.Create("测试钥匙")
	if err != nil {
		t.Fatal(err)
	}
	if !IsSubKey(plain) || !strings.HasPrefix(plain, "wbk_") {
		t.Fatalf("明文格式不对: %q", plain)
	}
	if k.Hash == plain {
		t.Fatal("服务端存了明文，应只存摘要")
	}
	got, err := s.Verify(plain, "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != k.ID {
		t.Fatal("校验未命中刚创建的钥匙")
	}
	// 非 wbk_ 前缀：不由本包处理。
	got, err = s.Verify("sk-xxx", "1.2.3.4")
	if err != nil || got != nil {
		t.Fatal("非子钥匙应返回 (nil, nil)")
	}
	// 伪造 token：401。
	if _, err = s.Verify("wbk_"+"00", "1.2.3.4"); !isCode(err, 401) {
		t.Fatalf("伪造 token 应 401，实际 %v", err)
	}
}

// 停用 → 403；过期 → 403；配额用尽 → 429。
func TestVerifyStatusCodes(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	plain, k, _ := s.Create("状态测试")

	k.Disabled = true
	if _, err := s.Verify(plain, ""); !isCode(err, 403) {
		t.Fatalf("停用应 403，实际 %v", err)
	}
	k.Disabled = false

	k.ExpiresAt = time.Now().Add(-time.Hour)
	if _, err := s.Verify(plain, ""); !isCode(err, 403) {
		t.Fatalf("过期应 403，实际 %v", err)
	}
	k.ExpiresAt = time.Time{}

	k.Quota = 10
	k.Used = 10
	if _, err := s.Verify(plain, ""); !isCode(err, 429) {
		t.Fatalf("配额用尽应 429，实际 %v", err)
	}
	k.Quota = -1
}

// IP 白名单：精确 IP + CIDR；命中外 → 400。
func TestIPAllowlist(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	plain, k, _ := s.Create("IP测试")
	k.IPAllow = []string{"10.0.0.5", "192.168.1.0/24"}

	if _, err := s.Verify(plain, "10.0.0.5"); err != nil {
		t.Fatalf("白名单 IP 应通过: %v", err)
	}
	if _, err := s.Verify(plain, "192.168.1.77"); err != nil {
		t.Fatalf("CIDR 内 IP 应通过: %v", err)
	}
	if _, err := s.Verify(plain, "8.8.8.8:1234"); !isCode(err, 400) {
		t.Fatalf("白名单外 IP 应 400，实际 %v", err)
	}
}

// 模型白名单：空=全放行；cn: 前缀剥离；"cn:" 后缀通配。
func TestModelWhitelist(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	_, k, _ := s.Create("模型测试")

	if err := s.VerifyRequest(k, "any-model"); err != nil {
		t.Fatalf("空名单应全放行: %v", err)
	}
	k.Models = []string{"gpt-4", "cn:"}
	if err := s.VerifyRequest(k, "gpt-4"); err != nil {
		t.Fatalf("名单内模型应通过: %v", err)
	}
	if err := s.VerifyRequest(k, "cn:deepseek-v3"); err != nil {
		t.Fatalf("cn: 前缀通配应通过: %v", err)
	}
	if err := s.VerifyRequest(k, "claude-3"); !isCode(err, 400) {
		t.Fatalf("名单外模型应 400，实际 %v", err)
	}
}

// 额度扣减累计。
func TestConsume(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	_, k, _ := s.Create("额度测试")
	if err := s.Consume(k.ID, 3.5, 100); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(k.ID)
	if got.Used != 103.5 {
		t.Fatalf("Used 应为 103.5，实际 %v", got.Used)
	}
}

// 落盘重载：摘要索引重建。
func TestReload(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	plain, k, _ := s.Create("重载测试")

	s2, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Verify(plain, "")
	if err != nil || got.ID != k.ID {
		t.Fatalf("重载后校验失败: %v", err)
	}
}

func isCode(err error, status int) bool {
	e, ok := err.(*AuthError)
	return ok && e.Status == status
}
