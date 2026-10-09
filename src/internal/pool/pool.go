// Package pool 实现 §5 的账号池：加载/回写账号文件、选号、单飞刷新、冷却。
//
// 硬约束：持锁期间不做网络 IO、不写盘、不打日志（SPEC §2.6）。
// 因此网络刷新与文件回写都在锁外完成；状态变更用主 mutex 保护，文件写入用
// 独立的 persistMu 串行化。
package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"xhx2api/internal/logx"
	"xhx2api/internal/upstream"
)

// Account 是账号文件里的一条账号记录。
//
// 未知字段通过 extra 原样保留，回写时不丢（SPEC §5.1）。
type Account struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	UserID        string `json:"user_id"`
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token"`
	ExpiresAt     int64  `json:"expires_at"`
	Enabled       bool   `json:"enabled"`
	Priority      int    `json:"priority"`
	LastRefreshAt int64  `json:"last_refresh_at"`
	CooldownUntil int64  `json:"cooldown_until"`
	LastError     string `json:"last_error"`

	extra map[string]json.RawMessage
}

// UnmarshalJSON 保留全部原始字段（未知字段回写时不丢）。
func (a *Account) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	type alias Account
	var tmp alias
	if err := json.Unmarshal(data, &tmp); err != nil {
		return err
	}
	*a = Account(tmp)
	if _, ok := raw["enabled"]; !ok {
		a.Enabled = true // 缺省启用
	}
	a.extra = raw
	return nil
}

// MarshalJSON 输出已知字段 + 未知字段。
func (a Account) MarshalJSON() ([]byte, error) {
	type alias Account
	base, err := json.Marshal(alias(a))
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(base, &fields); err != nil {
		return nil, err
	}
	for key, value := range a.extra {
		if _, ok := fields[key]; !ok {
			fields[key] = value
		}
	}
	return json.Marshal(fields)
}

func (a *Account) clone() *Account {
	if a == nil {
		return nil
	}
	dup := *a
	if a.extra != nil {
		dup.extra = make(map[string]json.RawMessage, len(a.extra))
		for key, value := range a.extra {
			dup.extra[key] = value
		}
	}
	return &dup
}

// Config 是账号池配置（由 §3 accounts 换算）。
type Config struct {
	File                 string
	Strategy             string
	RefreshMarginSeconds int
	CooldownSeconds      int
	MaxAttempts          int
	RefreshOn401         bool
}

// Pool 是进程内共享的账号池。
type Pool struct {
	mu        sync.Mutex
	persistMu sync.Mutex

	cfg    Config
	client *upstream.Client
	logger *logx.Logger

	accounts []*Account
	rr       uint64
	inflight map[string]*refreshFlight
}

type refreshFlight struct {
	done      chan struct{}
	access    string
	refresh   string
	expiresAt int64
	err       error
}

// New 构造账号池并加载账号文件（文件不存在只警告，不失败）。
func New(cfg Config, client *upstream.Client, logger *logx.Logger) *Pool {
	p := &Pool{
		cfg:      cfg,
		client:   client,
		logger:   logger,
		inflight: map[string]*refreshFlight{},
	}
	p.load()
	return p
}

func (p *Pool) load() {
	data, err := os.ReadFile(p.cfg.File)
	if err != nil {
		if os.IsNotExist(err) {
			p.logger.Warnf("[pool] 警告：账号文件 %s 不存在，服务将以空账号池启动（任何 /v1 请求将返回 503）", p.cfg.File)
			return
		}
		p.logger.Warnf("[pool] 警告：读取账号文件失败（%s）：%v", p.cfg.File, err)
		return
	}
	var file struct {
		Accounts []*Account `json:"accounts"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		p.logger.Warnf("[pool] 警告：解析账号文件失败（%s）：%v", p.cfg.File, err)
		return
	}
	p.accounts = file.Accounts
}

// persist 把账号池原子写回文件（临时文件 + rename，权限 0600）。
//
// 快照在 mu 内完成（只做序列化，无 IO），文件写入在锁外完成；
// 写失败只记日志，不影响本次请求（SPEC §5.1）。
func (p *Pool) persist() {
	p.persistMu.Lock()
	defer p.persistMu.Unlock()

	p.mu.Lock()
	payload := struct {
		Accounts []*Account `json:"accounts"`
	}{Accounts: p.accounts}
	data, err := json.MarshalIndent(payload, "", "  ")
	p.mu.Unlock()
	if err != nil {
		p.logger.Warnf("[pool] 警告：序列化账号文件失败：%v", err)
		return
	}
	data = append(data, '\n')

	dir := filepath.Dir(p.cfg.File)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			p.logger.Warnf("[pool] 警告：创建账号目录失败（%s）：%v", dir, err)
			return
		}
	}
	tmp, err := os.CreateTemp(dir, ".accounts-*.tmp")
	if err != nil {
		p.logger.Warnf("[pool] 警告：创建临时账号文件失败：%v", err)
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		p.logger.Warnf("[pool] 警告：写入临时账号文件失败：%v", err)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		p.logger.Warnf("[pool] 警告：关闭临时账号文件失败：%v", err)
		return
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		p.logger.Warnf("[pool] 警告：设置账号文件权限失败：%v", err)
	}
	if err := os.Rename(tmpName, p.cfg.File); err != nil {
		os.Remove(tmpName)
		p.logger.Warnf("[pool] 警告：替换账号文件失败：%v", err)
	}
}

func (p *Pool) findLocked(id string) *Account {
	for _, account := range p.accounts {
		if account.ID == id {
			return account
		}
	}
	return nil
}

func (p *Pool) expiring(expiresAt int64, now int64) bool {
	if expiresAt <= 0 {
		return false // 过期时间未知 → 不主动刷新，靠 401 触发
	}
	margin := int64(p.cfg.RefreshMarginSeconds) * 1000
	return now+margin >= expiresAt
}

// Pick 选一个可用账号（返回副本，不在锁外暴露内部指针）。
//
// 跳过 enabled=false、冷却中、无 access_token 的账号；exclude 里的 id 也跳过。
// priority 策略按 priority 升序（相同按文件顺序）；round_robin 在可用集合上轮转。
func (p *Pool) Pick(exclude map[string]bool) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UnixMilli()
	available := make([]*Account, 0, len(p.accounts))
	for _, account := range p.accounts {
		if !account.Enabled || account.AccessToken == "" {
			continue
		}
		if account.CooldownUntil > now {
			continue
		}
		if exclude != nil && exclude[account.ID] {
			continue
		}
		available = append(available, account)
	}
	if len(available) == 0 {
		return nil
	}
	var chosen *Account
	if p.cfg.Strategy == "round_robin" {
		chosen = available[int(p.rr%uint64(len(available)))]
		p.rr++
	} else {
		chosen = available[0]
		for _, account := range available[1:] {
			if account.Priority < chosen.Priority {
				chosen = account
			}
		}
	}
	return chosen.clone()
}

// Token 返回指定账号当前可用的 access_token。
//
// force=false 时仅在临期（expires_at > 0 且 now+margin >= expires_at）时刷新；
// force=true 时无条件刷新（401 后的强制刷新）。刷新失败不覆盖旧 token，记录
// last_error 并返回错误。同一账号的并发刷新只发一次真实请求（单飞）。
func (p *Pool) Token(ctx context.Context, id string, force bool) (string, error) {
	p.mu.Lock()
	account := p.findLocked(id)
	if account == nil {
		p.mu.Unlock()
		return "", fmt.Errorf("账号 %s 不存在", id)
	}
	token := account.AccessToken
	refreshToken := account.RefreshToken
	expiresAt := account.ExpiresAt
	needRefresh := force || p.expiring(expiresAt, time.Now().UnixMilli())
	if !needRefresh || refreshToken == "" {
		p.mu.Unlock()
		return token, nil
	}
	// 单飞：已有进行中的刷新则等待其结果。
	if flight, ok := p.inflight[id]; ok {
		p.mu.Unlock()
		p.logger.Debugf("[pool] 账号 %s 复用进行中的刷新（单飞等待）", id)
		<-flight.done
		if flight.err != nil {
			return "", flight.err
		}
		return flight.access, nil
	}
	flight := &refreshFlight{done: make(chan struct{})}
	p.inflight[id] = flight
	p.mu.Unlock()

	access, newRefresh, err := p.client.Refresh(ctx, refreshToken)

	p.mu.Lock()
	if err == nil {
		if account := p.findLocked(id); account != nil {
			account.AccessToken = access
			if newRefresh != "" {
				account.RefreshToken = newRefresh
			}
			if exp := upstream.JWTExpiry(access); exp > 0 {
				account.ExpiresAt = exp
			}
			account.LastRefreshAt = time.Now().UnixMilli()
			account.LastError = ""
			flight.access = access
		} else {
			err = fmt.Errorf("账号 %s 在刷新期间被移除", id)
		}
	} else if account := p.findLocked(id); account != nil {
		account.LastError = err.Error()
	}
	flight.err = err
	delete(p.inflight, id)
	close(flight.done)
	p.mu.Unlock()

	p.persist()
	if err != nil {
		return "", err
	}
	return access, nil
}

// Cooldown 把账号冷却 cooldown_seconds。
func (p *Pool) Cooldown(id string) {
	p.mu.Lock()
	if account := p.findLocked(id); account != nil {
		account.CooldownUntil = time.Now().UnixMilli() + int64(p.cfg.CooldownSeconds)*1000
		account.LastError = "上游返回 429，账号已冷却"
	}
	p.mu.Unlock()
	p.persist()
}

// MarkError 记录账号最近一次错误（不冷却）。
func (p *Pool) MarkError(id, message string) {
	p.mu.Lock()
	if account := p.findLocked(id); account != nil {
		account.LastError = message
	}
	p.mu.Unlock()
	p.persist()
}

// FirstToken 返回首个启用账号的 access_token（用于目录刷新等只读动作）；没有则空串。
func (p *Pool) FirstToken() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, account := range p.accounts {
		if account.Enabled && account.AccessToken != "" {
			return account.AccessToken
		}
	}
	return ""
}

// Counts 返回 total/enabled/available/cooldown 计数。
func (p *Pool) Counts() (total, enabled, available, cooldown int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UnixMilli()
	for _, account := range p.accounts {
		total++
		if !account.Enabled {
			continue
		}
		enabled++
		if account.CooldownUntil > now {
			cooldown++
			continue
		}
		if account.AccessToken != "" {
			available++
		}
	}
	return
}

// AccountView 是账号的只读快照（供 /healthz、§7.1 使用）。
type AccountView struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	UserID           string `json:"user_id"`
	Enabled          bool   `json:"enabled"`
	Priority         int    `json:"priority"`
	TokenTail        string `json:"token_tail"`
	ExpiresAt        int64  `json:"expires_at"`
	ExpiresInSeconds int64  `json:"expires_in_seconds"`
	Expired          bool   `json:"expired"`
	CooldownUntil    int64  `json:"cooldown_until"`
	LastRefreshAt    int64  `json:"last_refresh_at"`
	LastError        string `json:"last_error"`
	HasRefreshToken  bool   `json:"has_refresh_token"`
}

// Views 返回全部账号的只读快照。
func (p *Pool) Views() []AccountView {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UnixMilli()
	views := make([]AccountView, 0, len(p.accounts))
	for _, account := range p.accounts {
		view := AccountView{
			ID:              account.ID,
			Name:            account.Name,
			UserID:          account.UserID,
			Enabled:         account.Enabled,
			Priority:        account.Priority,
			TokenTail:       upstream.TokenTail(account.AccessToken),
			ExpiresAt:       account.ExpiresAt,
			CooldownUntil:   account.CooldownUntil,
			LastRefreshAt:   account.LastRefreshAt,
			LastError:       account.LastError,
			HasRefreshToken: account.RefreshToken != "",
		}
		if account.ExpiresAt > 0 {
			view.ExpiresInSeconds = (account.ExpiresAt - now) / 1000
			view.Expired = account.ExpiresAt <= now
		}
		views = append(views, view)
	}
	return views
}

// TokenFor 返回账号当前 token（不刷新），供只读查询使用；不存在返回空串。
func (p *Pool) TokenFor(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if account := p.findLocked(id); account != nil {
		return account.AccessToken
	}
	return ""
}
