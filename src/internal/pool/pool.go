// Package pool 实现 §5 的账号池：加载/回写账号文件、选号、单飞刷新、冷却。
//
// 硬约束：持锁期间不做网络 IO、不写盘、不打日志（SPEC §2.6）。
// 因此网络刷新与文件回写都在锁外完成；状态变更用主 mutex 保护，文件写入用
// 独立的 persistMu 串行化。
package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"xhx2api/internal/logx"
	"xhx2api/internal/upstream"
)

// 账号池写操作错误（SPEC4 §G.2）。
var (
	// ErrAccountExists 表示要新增的账号 id 已存在。
	ErrAccountExists = errors.New("账号 id 已存在")
	// ErrAccountNotFound 表示账号不存在。
	ErrAccountNotFound = errors.New("账号不存在")
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

// SetExtra 设置一个未知字段（回写账号文件时原样保留）。
//
// 网页登录换回的 office_identity / office_org_name / office_org_role 用它落盘
// （SPEC5 §K2），与账号文件里既有的未知字段走同一份存储。
func (a *Account) SetExtra(key string, value json.RawMessage) {
	if a.extra == nil {
		a.extra = map[string]json.RawMessage{}
	}
	a.extra[key] = value
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
// 返回错误供写接口判断（SPEC4 §G.2），现有调用点可忽略返回值。
func (p *Pool) persist() error {
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
		return fmt.Errorf("序列化账号文件失败：%v", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(p.cfg.File)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			p.logger.Warnf("[pool] 警告：创建账号目录失败（%s）：%v", dir, err)
			return fmt.Errorf("创建账号目录失败：%v", err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".accounts-*.tmp")
	if err != nil {
		p.logger.Warnf("[pool] 警告：创建临时账号文件失败：%v", err)
		return fmt.Errorf("创建临时账号文件失败：%v", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		p.logger.Warnf("[pool] 警告：写入临时账号文件失败：%v", err)
		return fmt.Errorf("写入临时账号文件失败：%v", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		p.logger.Warnf("[pool] 警告：关闭临时账号文件失败：%v", err)
		return fmt.Errorf("关闭临时账号文件失败：%v", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		p.logger.Warnf("[pool] 警告：设置账号文件权限失败：%v", err)
	}
	if err := os.Rename(tmpName, p.cfg.File); err != nil {
		os.Remove(tmpName)
		p.logger.Warnf("[pool] 警告：替换账号文件失败：%v", err)
		return fmt.Errorf("替换账号文件失败：%v", err)
	}
	return nil
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

// viewOf 由账号构造只读快照（Views / View / 写接口响应复用同一份）。
func viewOf(account *Account, now int64) AccountView {
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
	return view
}

// Views 返回全部账号的只读快照。
func (p *Pool) Views() []AccountView {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UnixMilli()
	views := make([]AccountView, 0, len(p.accounts))
	for _, account := range p.accounts {
		views = append(views, viewOf(account, now))
	}
	return views
}

// View 返回单个账号的只读快照；不存在返回 false。
func (p *Pool) View(id string) (AccountView, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.findLocked(id)
	if account == nil {
		return AccountView{}, false
	}
	return viewOf(account, time.Now().UnixMilli()), true
}

// AccountPatch 描述一次账号更新；nil 字段保持不变（SPEC4 §G.2）。
type AccountPatch struct {
	Name         *string
	Priority     *int
	Enabled      *bool
	AccessToken  *string
	RefreshToken *string
}

// Add 新增账号并原子回写；id 冲突返回 ErrAccountExists。持锁期间不写盘。
func (p *Pool) Add(account *Account) error {
	if account == nil {
		return errors.New("账号为空")
	}
	p.mu.Lock()
	if p.findLocked(account.ID) != nil {
		p.mu.Unlock()
		return ErrAccountExists
	}
	p.accounts = append(p.accounts, account.clone())
	p.mu.Unlock()
	return p.persist()
}

// Upsert 按 id 新增或更新账号（SPEC5 §K2 网页登录落账号）。
//
// 已存在时只更新 token / expires_at / user_id 与未知字段（office_*），
// 保留 name / enabled / priority 等用户设置；返回 updated=true。
// 不存在时按新增处理，返回 updated=false。持锁期间不写盘。
func (p *Pool) Upsert(account *Account) (bool, error) {
	if account == nil {
		return false, errors.New("账号为空")
	}
	p.mu.Lock()
	existing := p.findLocked(account.ID)
	updated := existing != nil
	if updated {
		existing.AccessToken = account.AccessToken
		if account.RefreshToken != "" {
			existing.RefreshToken = account.RefreshToken
		}
		existing.ExpiresAt = account.ExpiresAt
		existing.UserID = account.UserID
		existing.LastError = ""
		for key, value := range account.extra {
			if existing.extra == nil {
				existing.extra = map[string]json.RawMessage{}
			}
			existing.extra[key] = value
		}
	} else {
		p.accounts = append(p.accounts, account.clone())
	}
	p.mu.Unlock()
	return updated, p.persist()
}

// Update 只改给出的字段并原子回写；账号不存在返回 ErrAccountNotFound。
//
// 更新 access_token 时同步重算 expires_at 与 user_id（SPEC4 §G.2）。
func (p *Pool) Update(id string, patch AccountPatch) error {
	p.mu.Lock()
	account := p.findLocked(id)
	if account == nil {
		p.mu.Unlock()
		return ErrAccountNotFound
	}
	if patch.Name != nil {
		account.Name = *patch.Name
	}
	if patch.Priority != nil {
		account.Priority = *patch.Priority
	}
	if patch.Enabled != nil {
		account.Enabled = *patch.Enabled
	}
	if patch.RefreshToken != nil {
		account.RefreshToken = *patch.RefreshToken
	}
	if patch.AccessToken != nil {
		account.AccessToken = *patch.AccessToken
		account.ExpiresAt = upstream.JWTExpiry(*patch.AccessToken)
		account.UserID = upstream.JWTUserID(*patch.AccessToken)
	}
	p.mu.Unlock()
	return p.persist()
}

// Remove 删除账号并原子回写；账号不存在返回 ErrAccountNotFound。
func (p *Pool) Remove(id string) error {
	p.mu.Lock()
	index := -1
	for i, account := range p.accounts {
		if account.ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		p.mu.Unlock()
		return ErrAccountNotFound
	}
	p.accounts = append(p.accounts[:index], p.accounts[index+1:]...)
	p.mu.Unlock()
	return p.persist()
}

// TestToken 用账号当前 token 只读地打一次上游目录，返回状态码与模型条数。
//
// 不刷新 token、不写回（SPEC4 §G.2）。账号不存在返回 ErrAccountNotFound。
func (p *Pool) TestToken(ctx context.Context, id string) (int, int, error) {
	p.mu.Lock()
	account := p.findLocked(id)
	if account == nil {
		p.mu.Unlock()
		return 0, 0, ErrAccountNotFound
	}
	token := account.AccessToken
	p.mu.Unlock()
	if token == "" {
		return 0, 0, errors.New("账号没有可用 token")
	}
	return p.client.CatalogProbe(ctx, token)
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
