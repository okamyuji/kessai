// Package statemachine 決済の状態×イベント遷移テーブル駆動を提供します（ADR-0011）。
// 許可遷移だけを宣言し、テーブルにない組み合わせはすべてErrInvalidTransitionです。
// テーブルは docs/03-basic-design.md 2.1節の表と1対1対応します。
package statemachine

import (
	"errors"
	"fmt"
)

// State 決済の状態
type State string

// 8状態
const (
	StateCreated           State = "created"
	StateAuthorized        State = "authorized"
	StateCaptured          State = "captured"
	StateCanceled          State = "canceled"
	StateExpired           State = "expired"
	StateFailed            State = "failed"
	StatePartiallyRefunded State = "partially_refunded"
	StateRefunded          State = "refunded"
)

// Event 状態遷移を引き起こすイベント
type Event string

// 7イベント
const (
	EventAuthorizeSucceeded Event = "AuthorizeSucceeded"
	EventAuthorizeFailed    Event = "AuthorizeFailed"
	EventCapture            Event = "Capture"
	EventCancel             Event = "Cancel"
	EventExpire             Event = "Expire"
	EventRefundPartial      Event = "RefundPartial"
	EventRefundFull         Event = "RefundFull"
)

// AllStates 全状態を宣言順で返します（テスト網羅用）
func AllStates() []State {
	return []State{
		StateCreated, StateAuthorized, StateCaptured, StateCanceled,
		StateExpired, StateFailed, StatePartiallyRefunded, StateRefunded,
	}
}

// AllEvents 全イベントを宣言順で返します（テスト網羅用）
func AllEvents() []Event {
	return []Event{
		EventAuthorizeSucceeded, EventAuthorizeFailed, EventCapture, EventCancel,
		EventExpire, EventRefundPartial, EventRefundFull,
	}
}

// TerminalStates 終端状態集合
func TerminalStates() map[State]struct{} {
	return map[State]struct{}{
		StateCanceled: {}, StateExpired: {}, StateFailed: {}, StateRefunded: {},
	}
}

// IsTerminal stateが終端状態かを判定します
func IsTerminal(s State) bool { _, ok := TerminalStates()[s]; return ok }

// transition キーは (from, event)、値は to
type transitionKey struct {
	from  State
	event Event
}

var table = map[transitionKey]State{
	// createdから
	{StateCreated, EventAuthorizeSucceeded}: StateAuthorized,
	{StateCreated, EventAuthorizeFailed}:    StateFailed,
	{StateCreated, EventExpire}:             StateExpired,

	// authorizedから
	{StateAuthorized, EventCapture}: StateCaptured,
	{StateAuthorized, EventCancel}:  StateCanceled,
	{StateAuthorized, EventExpire}:  StateExpired,

	// capturedから（返金）
	{StateCaptured, EventRefundPartial}: StatePartiallyRefunded,
	{StateCaptured, EventRefundFull}:    StateRefunded,

	// partially_refundedから
	{StatePartiallyRefunded, EventRefundPartial}: StatePartiallyRefunded,
	{StatePartiallyRefunded, EventRefundFull}:    StateRefunded,
}

// ErrInvalidTransition 許可されていない遷移
var ErrInvalidTransition = errors.New("statemachine: 許可されていない遷移")

// Next 現在状態とイベントから次状態を返します。許可外はErrInvalidTransitionです。
func Next(from State, event Event) (State, error) {
	if to, ok := table[transitionKey{from, event}]; ok {
		return to, nil
	}
	return "", fmt.Errorf("%w: %s + %s", ErrInvalidTransition, from, event)
}

// IsAllowed Next相当の判定を真偽値で返します（テスト網羅・UI表示向け）
func IsAllowed(from State, event Event) bool {
	_, ok := table[transitionKey{from, event}]
	return ok
}

// TransitionsFrom fromから許可される (event, to) の一覧を返します
func TransitionsFrom(from State) map[Event]State {
	out := map[Event]State{}
	for k, v := range table {
		if k.from == from {
			out[k.event] = v
		}
	}
	return out
}
