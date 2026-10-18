package panel

import (
	"errors"
	"net/http"
	"time"

	"workbuddy2api-prime/internal/types"
)

// Package accounts 提供账号运维接口：单号签到 / 余额刷新 / 批量操作 /
// 停用启用 / 删除。运维语义对齐 linguo panel.go:461-660：
//   - 签到：DailyCheckin + 无论成败都刷新余额（已签到等业务错误不阻塞）；
//   - 余额：只刷新余额与到期快照，不触碰冷却状态；
//   - 停用：退出选号候选，但照常参与签到/保活等维护任务；
//     启用即恢复（幂等）；删除：出池 + 删凭证文件。

// findAccount 按 ID 取池内 live 账号指针。
func (p *Panel) findAccount(uid string) *types.Account {
	if p.pool == nil {
		return nil
	}
	return p.pool.Get(uid)
}

// refreshBalanceLocked 拉取单个账号余额并写回（内存 + 落盘）。
// 返回 remain/total/earliest。调用方已持有账号指针。
func (p *Panel) refreshBalanceLocked(acc *types.Account) (remain, total int64, err error) {
	if p.billing == nil {
		return 0, 0, errBillingNA
	}
	remain, total, earliest, err := p.billing.FetchBalance(acc)
	if err != nil {
		return 0, 0, err
	}
	acc.CreditTotal = float64(remain)
	if !earliest.IsZero() {
		acc.CreditExpiresAt = earliest
	}
	acc.UpdatedAt = time.Now()
	if p.auth != nil {
		if serr := p.auth.Save(acc); serr != nil {
			return remain, total, serr
		}
	}
	return remain, total, nil
}

var errBillingNA = errors.New("billing 客户端未初始化")

// handleAccountCheckin: POST /panel/api/accounts/{uid}/checkin
// 单号签到：签到 + 余额刷新，结果记入签到历史。
func (p *Panel) handleAccountCheckin(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	acc := p.findAccount(uid)
	if acc == nil {
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	if p.sched == nil {
		writeError(w, http.StatusNotImplemented, "scheduler 未初始化")
		return
	}
	res := p.sched.CheckinAccount(r.Context(), acc)
	p.sched.RecordCheckin(res)
	writeJSON(w, map[string]any{
		"ok":      res.OK,
		"detail":  res.Detail,
		"credits": acc.CreditTotal,
	})
}

// handleAccountBalance: POST /panel/api/accounts/{uid}/balance
// 单号余额刷新：不触碰冷却状态。
func (p *Panel) handleAccountBalance(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	acc := p.findAccount(uid)
	if acc == nil {
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	remain, total, err := p.refreshBalanceLocked(acc)
	if err != nil {
		writeError(w, http.StatusBadGateway, "余额刷新失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "credits": remain, "credits_total": total})
}

// handleCheckinAll: POST /panel/api/checkin_all
// 全量签到：异步触发定时签到任务（账号间已有 2–5s 错峰）。
func (p *Panel) handleCheckinAll(w http.ResponseWriter, r *http.Request) {
	if p.sched == nil {
		writeError(w, http.StatusNotImplemented, "scheduler 未初始化")
		return
	}
	if err := p.sched.TriggerNow("checkin"); err != nil {
		writeError(w, http.StatusBadRequest, "触发失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "started": true})
}

// handleBalanceAll: POST /panel/api/balance_all
// 全量余额刷新：同步执行，账号间 2s 错峰（防风控），返回逐账号结果。
func (p *Panel) handleBalanceAll(w http.ResponseWriter, r *http.Request) {
	if p.pool == nil {
		writeError(w, http.StatusNotImplemented, "pool 未初始化")
		return
	}
	type one struct {
		UID     string `json:"uid"`
		OK      bool   `json:"ok"`
		Credits int64  `json:"credits,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	accs := p.pool.All()
	out := make([]one, 0, len(accs))
	for i, acc := range accs {
		if acc == nil || acc.Disabled {
			continue
		}
		if i > 0 {
			select {
			case <-r.Context().Done():
				writeJSON(w, map[string]any{"ok": true, "results": out, "interrupted": true})
				return
			case <-time.After(2 * time.Second):
			}
		}
		remain, _, err := p.refreshBalanceLocked(acc)
		o := one{UID: acc.ID}
		if err != nil {
			o.Error = err.Error()
		} else {
			o.OK = true
			o.Credits = remain
		}
		out = append(out, o)
	}
	writeJSON(w, map[string]any{"ok": true, "results": out})
}

// handleAccountDisable: POST /panel/api/accounts/{uid}/disable
// handleAccountEnable: POST /panel/api/accounts/{uid}/enable
// 停用/启用：退出/恢复选号候选，维护任务照常。
func (p *Panel) handleAccountDisable(w http.ResponseWriter, r *http.Request) {
	p.setAccountDisabled(w, r, true)
}

func (p *Panel) handleAccountEnable(w http.ResponseWriter, r *http.Request) {
	p.setAccountDisabled(w, r, false)
}

func (p *Panel) setAccountDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	uid := r.PathValue("uid")
	if p.pool == nil || !p.pool.SetDisabled(uid, disabled) {
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	if acc := p.pool.Get(uid); acc != nil && p.auth != nil {
		_ = p.auth.Save(acc) // 落盘失败不阻塞本次操作（内存态已生效）
	}
	writeJSON(w, map[string]any{"ok": true, "disabled": disabled})
}

// handleAccountRemove: DELETE /panel/api/accounts/{uid}
// 删除账号：出池 + 删凭证文件。前端二次确认。
func (p *Panel) handleAccountRemove(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if p.pool == nil {
		writeError(w, http.StatusNotImplemented, "pool 未初始化")
		return
	}
	acc := p.pool.Remove(uid)
	if acc == nil {
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	fileErr := ""
	if p.auth != nil {
		if err := p.auth.Delete(uid); err != nil {
			fileErr = err.Error()
		}
	}
	resp := map[string]any{"ok": true}
	if fileErr != "" {
		resp["file_error"] = fileErr
	}
	writeJSON(w, resp)
}
