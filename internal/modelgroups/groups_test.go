package modelgroups

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreCRUD(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if got := len(s.List()); got != 0 {
		t.Fatalf("初始应为空，got %d", got)
	}
	// Create
	g, err := s.Create("便宜组", []string{"hy3", "glm-5.2"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if g.Name != "便宜组" || len(g.Models) != 2 {
		t.Fatalf("Create 返回异常: %+v", g)
	}
	// 空名应报错
	if _, err := s.Create("", nil); err == nil {
		t.Fatal("空名应报错")
	}
	// List
	if got := len(s.List()); got != 1 {
		t.Fatalf("List 应为 1，got %d", got)
	}
	// Update
	g2, err := s.Update(g.ID, "便宜组2", []string{"hy3"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if g2.Name != "便宜组2" || len(g2.Models) != 1 {
		t.Fatalf("Update 返回异常: %+v", g2)
	}
	// 持久化：重新打开应读到
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("重开 NewStore: %v", err)
	}
	got := s2.Get(g.ID)
	if got == nil || got.Name != "便宜组2" || len(got.Models) != 1 {
		t.Fatalf("持久化读取异常: %+v", got)
	}
	// Delete
	if err := s2.Delete(g.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(s2.List()) != 0 {
		t.Fatal("删除后应为空")
	}
	if err := s2.Delete("not-exist"); err == nil {
		t.Fatal("删除不存在的应报错")
	}
	// 文件权限 0600
	fi, err := os.Stat(filepath.Join(dir, "model_groups.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("权限应为 0600，got %o", fi.Mode().Perm())
	}
}
