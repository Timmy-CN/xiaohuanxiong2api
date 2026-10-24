// Package checkin 实现 SPEC2 §A 的每日签到：触发上游发放、账单核对、历史落盘与后台调度。
//
// 可测试性：取「现在」的时间函数通过结构体字段 clock 注入（默认 time.Now），
// 测试替换它即可模拟跨天；生产代码里没有任何 -test.* 旁路。
package checkin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"xhx2api/internal/logx"
	"xhx2api/internal/pool"
	"xhx2api/internal/upstream"
)

// Config 是签到服务的配置（由 §3 checkin 换算）。
type Config struct {
	Enabled           bool
	Time              string // "HH:MM"
	OnStart           bool
	VerifyBills       bool
	BillsLimit        int
	RetryInterval     time.Duration
	MaxAttemptsPerDay int
	HistoryFile       string
	HistoryDays       int
}

// Accounts 是签到所需的账号池能力（便于测试替换）。
type Accounts interface {
	Views() []pool.AccountView
	Token(ctx context.Context, id string, force bool) (string, error)
}

// Service 是每日签到服务。
type Service struct {
	cfg    Config
	client *upstream.Client
	pool   Accounts
	logger *logx.Logger

	clock   func() time.Time // 可注入时钟
	spacing time.Duration    // 账号之间的串行间隔，默认 1s

	mu    sync.Mutex
	state *State

	startOnce sync.Once
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

// New 构造签到服务并加载历史文件（文件不存在则从空状态开始）。
func New(cfg Config, client *upstream.Client, accounts Accounts, logger *logx.Logger) *Service {
	svc := &Service{
		cfg:     cfg,
		client:  client,
		pool:    accounts,
		logger:  logger,
		clock:   time.Now,
		spacing: time.Second,
		state:   loadState(cfg.HistoryFile),
	}
	return svc
}

// SetClock 替换取时函数（仅测试使用）。
func (s *Service) SetClock(fn func() time.Time) {
	if fn != nil {
		s.clock = fn
	}
}

func loadState(path string) *State {
	state := &State{Accounts: map[string]*AccountState{}}
	if path == "" {
		return state
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return state
	}
	if err := json.Unmarshal(data, state); err != nil {
		return &State{Accounts: map[string]*AccountState{}}
	}
	if state.Accounts == nil {
		state.Accounts = map[string]*AccountState{}
	}
	return state
}

// save 原子写回历史文件（临时文件 + rename，权限 0600）。
func (s *Service) save() {
	if s.cfg.HistoryFile == "" {
		return
	}
	s.mu.Lock()
	data, err := json.MarshalIndent(s.state, "", "  ")
	s.mu.Unlock()
	if err != nil {
		s.logger.Warnf("[Checkin] 序列化签到历史失败：%v", err)
		return
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.cfg.HistoryFile)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			s.logger.Warnf("[Checkin] 创建签到历史目录失败（%s）：%v", dir, err)
			return
		}
	}
	tmp, err := os.CreateTemp(dir, ".checkin-*.tmp")
	if err != nil {
		s.logger.Warnf("[Checkin] 创建临时签到文件失败：%v", err)
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		s.logger.Warnf("[Checkin] 写入临时签到文件失败：%v", err)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		s.logger.Warnf("[Checkin] 关闭临时签到文件失败：%v", err)
		return
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		s.logger.Warnf("[Checkin] 设置签到文件权限失败：%v", err)
	}
	if err := os.Rename(tmpName, s.cfg.HistoryFile); err != nil {
		os.Remove(tmpName)
		s.logger.Warnf("[Checkin] 替换签到文件失败：%v", err)
	}
}

// CheckAll 串行处理所有启用账号（间隔 spacing，默认 1 秒），每个账号当天最多成功一次。
func (s *Service) CheckAll(ctx context.Context) {
	views := s.pool.Views()
	first := true
	for _, view := range views {
		if !view.Enabled {
			continue
		}
		if !first {
			if !s.wait(ctx, s.spacing) {
				return
			}
		}
		first = false
		s.checkAccount(ctx, view)
	}
}

// wait 等待 d，期间收到 ctx 取消或 Stop 则返回 false。
func (s *Service) wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-s.stopCh:
		return false
	case <-timer.C:
		return true
	}
}

// checkAccount 处理单个账号的一次尝试。
func (s *Service) checkAccount(ctx context.Context, view pool.AccountView) {
	now := s.clock()
	today := now.Format("2006-01-02")

	s.mu.Lock()
	state := s.state.account(view.ID)
	if state.LastDate != today {
		state.AttemptsToday = 0
		state.LastDate = today
	}
	already := state.succeededOn(today) != nil
	exhausted := state.AttemptsToday >= s.cfg.MaxAttemptsPerDay
	if already || exhausted {
		s.mu.Unlock()
		if already {
			s.logger.Debugf("[Checkin] 账号 %s 今天已成功，跳过", view.ID)
		} else {
			s.logger.Debugf("[Checkin] 账号 %s 今日尝试次数已达上限（%d），跳过", view.ID, s.cfg.MaxAttemptsPerDay)
		}
		return
	}
	state.AttemptsToday++
	s.mu.Unlock()

	token, err := s.pool.Token(ctx, view.ID, false)
	if err != nil {
		s.recordFailure(view.ID, today, "刷新 token 失败："+err.Error())
		return
	}

	grant, err := s.client.ClaimDailyGrant(ctx, token)
	if err != nil {
		var statusErr *upstream.StatusError
		if errors.As(err, &statusErr) && statusErr.Status == 401 {
			refreshed, refreshErr := s.pool.Token(ctx, view.ID, true)
			if refreshErr != nil {
				s.recordFailure(view.ID, today, "强制刷新失败："+refreshErr.Error())
				return
			}
			token = refreshed
			grant, err = s.client.ClaimDailyGrant(ctx, token)
		}
	}
	if err != nil {
		s.recordFailure(view.ID, today, "触发发放失败："+err.Error())
		return
	}

	var popups []json.RawMessage
	var toast json.RawMessage
	if grant != nil {
		popups = grant.Popups
		toast = grant.Toast
		if len(toast) == 0 || string(toast) == "null" {
			toast = nil
		}
	}

	points := int64(0)
	msg := "已触发每日积分发放"
	if s.cfg.VerifyBills {
		bills, billErr := s.client.Bills(ctx, token, s.cfg.BillsLimit)
		if billErr != nil {
			s.recordFailure(view.ID, today, "账单核对失败："+billErr.Error())
			return
		}
		points = todayPoints(bills, today)
		if points <= 0 {
			s.recordFailure(view.ID, today, "今日未见积分入账")
			return
		}
		msg = fmt.Sprintf("今日积分 +%d", points)
	}
	// 一次性桌面端首登奖励：账单先读后领，奖励即使随后落成账单也不会混进
	// 上面的 points，消息里单列，不会重复计数。
	if onboardPts := s.claimOnboarding(ctx, view.ID, token); onboardPts > 0 {
		msg += fmt.Sprintf("；桌面端首次登录奖励 +%d", onboardPts)
	}
	s.recordSuccess(view.ID, today, points, msg, popups, toast)
}

// claimOnboarding 领取一次性桌面端首登奖励，返回本次领取到的积分（0 = 无可报）。
//
// 已结算（含上游 granted=false 的「早已领取」）后不再询问；查询失败保持未结算
// 下次签到再试，且绝不拦截本次签到。
func (s *Service) claimOnboarding(ctx context.Context, id, token string) int64 {
	s.mu.Lock()
	settled := s.state.account(id).OnboardingSettled
	s.mu.Unlock()
	if settled {
		return 0
	}

	grant, err := s.client.ClaimOnboardingGrant(ctx, token)
	if err != nil {
		var statusErr *upstream.StatusError
		if errors.As(err, &statusErr) && statusErr.Status == 401 {
			refreshed, refreshErr := s.pool.Token(ctx, id, true)
			if refreshErr != nil {
				s.logger.Debugf("[Checkin] 账号 %s 首登奖励：强制刷新失败（%v）", id, refreshErr)
				return 0
			}
			grant, err = s.client.ClaimOnboardingGrant(ctx, refreshed)
		}
	}
	if err != nil {
		s.logger.Debugf("[Checkin] 账号 %s 首登奖励查询失败（保持未结算）：%v", id, err)
		return 0
	}

	s.mu.Lock()
	s.state.account(id).OnboardingSettled = true
	s.mu.Unlock()
	s.save()
	if !grant.Granted {
		return 0
	}
	s.logger.Infof("[Checkin] 账号 %s 桌面端首次登录奖励 +%d", id, grant.Points)
	return grant.Points
}

// todayPoints 汇总今日入账：points > 0 且 created_at 前 10 位 == 本地当天。
func todayPoints(bills []upstream.Bill, today string) int64 {
	var total int64
	for _, bill := range bills {
		if bill.Points <= 0 {
			continue
		}
		if len(bill.CreatedAt) < 10 || bill.CreatedAt[:10] != today {
			continue
		}
		total += bill.Points
	}
	return total
}

func (s *Service) recordSuccess(id, today string, points int64, msg string, popups []json.RawMessage, toast json.RawMessage) {
	nowMs := s.clock().UnixMilli()
	s.mu.Lock()
	state := s.state.account(id)
	state.LastDate = today
	state.LastSuccessAt = nowMs
	state.LastMsg = msg
	state.LastError = ""
	state.addHistory(HistoryEntry{
		Date: today, Success: true, Points: points, Msg: msg, At: nowMs,
		Popups: popups, Toast: toast,
	}, s.cfg.HistoryDays)
	s.mu.Unlock()
	s.save()
	s.logger.Infof("[Checkin] 账号 %s %s", id, msg)
}

func (s *Service) recordFailure(id, today, reason string) {
	nowMs := s.clock().UnixMilli()
	s.mu.Lock()
	state := s.state.account(id)
	state.LastDate = today
	state.LastMsg = reason
	state.LastError = reason
	state.addHistory(HistoryEntry{Date: today, Success: false, Msg: reason, At: nowMs}, s.cfg.HistoryDays)
	s.mu.Unlock()
	s.save()
	s.logger.Warnf("[Checkin] 账号 %s 签到失败：%s", id, reason)
}

// ── 对外视图（SPEC2 §A.4）──

// Today 是某账号当天的签到结果。
type Today struct {
	Success bool              `json:"success"`
	Points  int64             `json:"points"`
	Msg     string            `json:"msg"`
	Popups  []json.RawMessage `json:"popups,omitempty"`
	Toast   json.RawMessage   `json:"toast,omitempty"`
}

// View 是 /admin/api/checkin 的单条记录。
type View struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	LastDate      string         `json:"last_date"`
	LastSuccessAt int64          `json:"last_success_at"`
	LastMsg       string         `json:"last_msg"`
	AttemptsToday int            `json:"attempts_today"`
	LastError     string         `json:"last_error"`
	Today         Today          `json:"today"`
	History       []HistoryEntry `json:"history"`
	SkippedReason string         `json:"skipped_reason"`
}

// Views 返回所有账号的签到视图（合并账号池的 id/name）。
func (s *Service) Views() []View {
	now := s.clock()
	today := now.Format("2006-01-02")
	poolViews := s.pool.Views()

	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]View, 0, len(poolViews))
	for _, pv := range poolViews {
		view := View{ID: pv.ID, Name: pv.Name, History: []HistoryEntry{}}
		state := s.state.Accounts[pv.ID]
		if state != nil {
			view.LastDate = state.LastDate
			view.LastSuccessAt = state.LastSuccessAt
			view.LastMsg = state.LastMsg
			view.AttemptsToday = state.AttemptsToday
			view.LastError = state.LastError
			if len(state.History) > 0 {
				view.History = state.History
			}
			if entry := state.entryOn(today); entry != nil {
				view.Today = Today{
					Success: entry.Success,
					Points:  entry.Points,
					Msg:     entry.Msg,
					Popups:  entry.Popups,
					Toast:   entry.Toast,
				}
			}
			switch {
			case state.succeededOn(today) != nil:
				view.SkippedReason = "今天已成功"
			case state.LastDate == today && state.AttemptsToday >= s.cfg.MaxAttemptsPerDay:
				view.SkippedReason = "今日尝试次数已达上限"
			}
		}
		result = append(result, view)
	}
	return result
}

// ── 后台调度（SPEC2 §A.2）──

// Start 启动后台 goroutine；enabled=false 时调用方不应调用。
func (s *Service) Start() {
	s.startOnce.Do(func() {
		s.stopCh = make(chan struct{})
		s.wg.Add(1)
		go s.loop()
	})
}

// Stop 停止后台 goroutine 并等待退出。
func (s *Service) Stop() {
	if s.stopCh == nil {
		return
	}
	select {
	case <-s.stopCh:
		// 已关闭
	default:
		close(s.stopCh)
	}
	s.wg.Wait()
}

func (s *Service) loop() {
	defer s.wg.Done()
	s.logger.Infof("[Checkin] 已启用：每日 %s 执行，on_start=%v verify_bills=%v", s.cfg.Time, s.cfg.OnStart, s.cfg.VerifyBills)
	if s.cfg.OnStart {
		s.CheckAll(context.Background())
	}
	for {
		delay := s.nextDelay()
		timer := time.NewTimer(delay)
		select {
		case <-s.stopCh:
			timer.Stop()
			return
		case <-timer.C:
			s.CheckAll(context.Background())
		}
	}
}

// nextDelay 返回距离下一次触发的时长（跨天会重新计算）。
func (s *Service) nextDelay() time.Duration {
	now := s.clock()
	next := s.nextDaily(now)
	if retry, ok := s.earliestRetry(now); ok && retry.Before(next) {
		next = retry
	}
	delay := next.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay
}

// nextDaily 返回下一个每日触发时刻（本地时区）。
func (s *Service) nextDaily(now time.Time) time.Time {
	hour, minute, err := parseClock(s.cfg.Time)
	if err != nil {
		hour, minute = 0, 1
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

// earliestRetry 返回当天失败账号中最早的重试时刻。
func (s *Service) earliestRetry(now time.Time) (time.Time, bool) {
	if s.cfg.RetryInterval <= 0 {
		return time.Time{}, false
	}
	today := now.Format("2006-01-02")
	s.mu.Lock()
	defer s.mu.Unlock()
	var earliest time.Time
	found := false
	for _, state := range s.state.Accounts {
		if state.LastDate != today || state.AttemptsToday == 0 {
			continue
		}
		if state.AttemptsToday >= s.cfg.MaxAttemptsPerDay || state.succeededOn(today) != nil {
			continue
		}
		at := state.lastAttemptAt(today)
		if at == 0 {
			continue
		}
		candidate := time.UnixMilli(at).Add(s.cfg.RetryInterval)
		if !found || candidate.Before(earliest) {
			earliest = candidate
			found = true
		}
	}
	return earliest, found
}

// parseClock 解析 HH:MM。
func parseClock(s string) (int, int, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, 0, errors.New("格式必须为 HH:MM")
	}
	hour, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, errors.New("小时必须是数字")
	}
	minute, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, errors.New("分钟必须是数字")
	}
	if hour < 0 || hour > 23 {
		return 0, 0, errors.New("小时必须在 00-23 之间")
	}
	if minute < 0 || minute > 59 {
		return 0, 0, errors.New("分钟必须在 00-59 之间")
	}
	return hour, minute, nil
}
