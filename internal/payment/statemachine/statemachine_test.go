package statemachine_test

import (
	"errors"
	"testing"

	sm "github.com/okamyuji/kessai/internal/payment/statemachine"
)

// docs/03-basic-design.md 2.1の表と1対1で対応する期待遷移。
// 表にない全セルはErrInvalidTransitionを期待します。
var expected = map[[2]string]sm.State{
	// created行
	{string(sm.StateCreated), string(sm.EventAuthorizeSucceeded)}: sm.StateAuthorized,
	{string(sm.StateCreated), string(sm.EventAuthorizeFailed)}:    sm.StateFailed,
	{string(sm.StateCreated), string(sm.EventExpire)}:             sm.StateExpired,
	// authorized行
	{string(sm.StateAuthorized), string(sm.EventCapture)}: sm.StateCaptured,
	{string(sm.StateAuthorized), string(sm.EventCancel)}:  sm.StateCanceled,
	{string(sm.StateAuthorized), string(sm.EventExpire)}:  sm.StateExpired,
	// captured行
	{string(sm.StateCaptured), string(sm.EventRefundPartial)}: sm.StatePartiallyRefunded,
	{string(sm.StateCaptured), string(sm.EventRefundFull)}:    sm.StateRefunded,
	// partially_refunded行
	{string(sm.StatePartiallyRefunded), string(sm.EventRefundPartial)}: sm.StatePartiallyRefunded,
	{string(sm.StatePartiallyRefunded), string(sm.EventRefundFull)}:    sm.StateRefunded,
}

func TestTransitionMatrix_AllCells(t *testing.T) {
	t.Parallel()
	states := sm.AllStates()
	events := sm.AllEvents()
	// 表の全セル（8×7=56）を検査。期待表になければInvalid、あれば期待toと一致。
	for _, s := range states {
		for _, e := range events {
			s, e := s, e
			key := [2]string{string(s), string(e)}
			want, allowed := expected[key]
			got, err := sm.Next(s, e)
			if allowed {
				if err != nil {
					t.Errorf("Next(%s,%s) err=%v want=%s", s, e, err, want)
				}
				if got != want {
					t.Errorf("Next(%s,%s)=%s want %s", s, e, got, want)
				}
			} else {
				if !errors.Is(err, sm.ErrInvalidTransition) {
					t.Errorf("Next(%s,%s) err=%v want ErrInvalidTransition", s, e, err)
				}
			}
		}
	}
}

func TestIsAllowed_ConsistentWithNext(t *testing.T) {
	t.Parallel()
	for _, s := range sm.AllStates() {
		for _, e := range sm.AllEvents() {
			_, err := sm.Next(s, e)
			nextAllowed := err == nil
			if sm.IsAllowed(s, e) != nextAllowed {
				t.Errorf("IsAllowed(%s,%s) と Next の判定が不一致", s, e)
			}
		}
	}
}

func TestTerminalStates(t *testing.T) {
	t.Parallel()
	terminal := map[sm.State]bool{
		sm.StateCanceled: true, sm.StateExpired: true, sm.StateFailed: true, sm.StateRefunded: true,
	}
	for _, s := range sm.AllStates() {
		got := sm.IsTerminal(s)
		if got != terminal[s] {
			t.Errorf("IsTerminal(%s)=%v want %v", s, got, terminal[s])
		}
		// 終端状態には出て行く遷移がない
		if got && len(sm.TransitionsFrom(s)) != 0 {
			t.Errorf("終端状態 %s から出る遷移がある: %v", s, sm.TransitionsFrom(s))
		}
	}
}

func TestReachability_AllStatesReachableFromCreated(t *testing.T) {
	t.Parallel()
	// BFSで到達可能状態集合を求める
	seen := map[sm.State]bool{sm.StateCreated: true}
	queue := []sm.State{sm.StateCreated}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, to := range sm.TransitionsFrom(cur) {
			if !seen[to] {
				seen[to] = true
				queue = append(queue, to)
			}
		}
	}
	for _, s := range sm.AllStates() {
		if !seen[s] {
			t.Errorf("状態 %s がcreatedから到達不能", s)
		}
	}
}

func TestNonTerminal_MustHaveExitEdge(t *testing.T) {
	t.Parallel()
	for _, s := range sm.AllStates() {
		if sm.IsTerminal(s) {
			continue
		}
		if len(sm.TransitionsFrom(s)) == 0 {
			t.Errorf("非終端 %s に脱出辺がない", s)
		}
	}
}
