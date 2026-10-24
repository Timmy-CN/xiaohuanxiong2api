package checkin

import (
	"encoding/json"
	"sort"
)

// HistoryEntry 是一条签到历史（SPEC2 §A.3）。
//
// 未知字段通过 extra 原样保留，回写时不丢。
type HistoryEntry struct {
	Date    string            `json:"date"`
	Success bool              `json:"success"`
	Points  int64             `json:"points"`
	Msg     string            `json:"msg"`
	At      int64             `json:"at"`
	Popups  []json.RawMessage `json:"popups,omitempty"`
	Toast   json.RawMessage   `json:"toast,omitempty"`

	extra map[string]json.RawMessage
}

// UnmarshalJSON 保留全部原始字段。
func (h *HistoryEntry) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	type alias HistoryEntry
	var tmp alias
	if err := json.Unmarshal(data, &tmp); err != nil {
		return err
	}
	*h = HistoryEntry(tmp)
	h.extra = raw
	return nil
}

// MarshalJSON 输出已知字段 + 未知字段。
func (h HistoryEntry) MarshalJSON() ([]byte, error) {
	type alias HistoryEntry
	base, err := json.Marshal(alias(h))
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(base, &fields); err != nil {
		return nil, err
	}
	for key, value := range h.extra {
		if _, ok := fields[key]; !ok {
			fields[key] = value
		}
	}
	return json.Marshal(fields)
}

// AccountState 是单个账号的签到状态（SPEC2 §A.3）。
type AccountState struct {
	LastDate      string         `json:"last_date"`
	LastSuccessAt int64          `json:"last_success_at"`
	LastMsg       string         `json:"last_msg"`
	AttemptsToday int            `json:"attempts_today"`
	LastError     string         `json:"last_error"`
	History       []HistoryEntry `json:"history"`
	// 桌面端首登奖励是一次性的：上游明确答复（无论发放与否）后不再询问。
	OnboardingSettled bool `json:"onboarding_settled"`

	extra map[string]json.RawMessage
}

// UnmarshalJSON 保留全部原始字段。
func (a *AccountState) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	type alias AccountState
	var tmp alias
	if err := json.Unmarshal(data, &tmp); err != nil {
		return err
	}
	*a = AccountState(tmp)
	a.extra = raw
	return nil
}

// MarshalJSON 输出已知字段 + 未知字段。
func (a AccountState) MarshalJSON() ([]byte, error) {
	type alias AccountState
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

// State 是签到历史文件（SPEC2 §A.3）。
type State struct {
	Accounts map[string]*AccountState `json:"accounts"`

	extra map[string]json.RawMessage
}

// UnmarshalJSON 保留全部原始字段。
func (s *State) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.extra = raw
	s.Accounts = map[string]*AccountState{}
	if accounts, ok := raw["accounts"]; ok {
		if err := json.Unmarshal(accounts, &s.Accounts); err != nil {
			return err
		}
	}
	return nil
}

// MarshalJSON 输出 accounts + 未知字段。
func (s State) MarshalJSON() ([]byte, error) {
	fields := map[string]json.RawMessage{}
	for key, value := range s.extra {
		if key == "accounts" {
			continue
		}
		fields[key] = value
	}
	accounts, err := json.Marshal(s.Accounts)
	if err != nil {
		return nil, err
	}
	fields["accounts"] = accounts
	return json.Marshal(fields)
}

// account 返回账号状态，不存在则创建。
func (s *State) account(id string) *AccountState {
	if s.Accounts == nil {
		s.Accounts = map[string]*AccountState{}
	}
	state, ok := s.Accounts[id]
	if !ok || state == nil {
		state = &AccountState{}
		s.Accounts[id] = state
	}
	return state
}

// succeededOn 返回指定日期已成功的记录，没有则 nil。
func (a *AccountState) succeededOn(day string) *HistoryEntry {
	for i := range a.History {
		if a.History[i].Date == day && a.History[i].Success {
			return &a.History[i]
		}
	}
	return nil
}

// entryOn 返回指定日期的最近一条记录（成功或失败），没有则 nil。
func (a *AccountState) entryOn(day string) *HistoryEntry {
	for i := len(a.History) - 1; i >= 0; i-- {
		if a.History[i].Date == day {
			return &a.History[i]
		}
	}
	return nil
}

// lastAttemptAt 返回指定日期最近一次尝试的时间戳（毫秒），没有则 0。
func (a *AccountState) lastAttemptAt(day string) int64 {
	if entry := a.entryOn(day); entry != nil {
		return entry.At
	}
	return 0
}

// addHistory 追加一条记录：按日期去重（新的覆盖旧的），并按 history_days 截断。
func (a *AccountState) addHistory(entry HistoryEntry, days int) {
	out := make([]HistoryEntry, 0, len(a.History)+1)
	for _, item := range a.History {
		if item.Date != entry.Date {
			out = append(out, item)
		}
	}
	out = append(out, entry)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	if days > 0 && len(out) > days {
		out = out[len(out)-days:]
	}
	a.History = out
}
