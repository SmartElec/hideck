package modemvoice

import (
	"context"
	"errors"
	"time"
)

func (s *Session) Dial(ctx context.Context, number string) (Update, error) {
	command, err := dialCommand(number)
	if err != nil {
		return Update{}, err
	}
	opCtx, release, err := s.enter(ctx)
	if err != nil {
		return Update{}, err
	}
	defer release()
	update, err := s.refresh(opCtx)
	if err != nil {
		return update, err
	}
	if len(s.voiceControlCalls()) != 0 {
		return update, errors.New("modem voice: device already has a call")
	}
	return s.execute(opCtx, command, update)
}

func (s *Session) Answer(ctx context.Context, id string) (Update, error) {
	return s.control(ctx, id, true)
}

func (s *Session) Hangup(ctx context.Context, id string) (Update, error) {
	return s.control(ctx, id, false)
}

func (s *Session) control(ctx context.Context, id string, answer bool) (Update, error) {
	opCtx, release, err := s.enter(ctx)
	if err != nil {
		return Update{}, err
	}
	defer release()
	if !containsCallID(s.tracker.Calls(), id) {
		return Update{}, ErrCallChanged
	}
	update, err := s.refresh(opCtx)
	if err != nil {
		return update, err
	}
	calls := s.voiceControlCalls()
	target, ok := trackedByID(calls, id)
	if !ok {
		return update, ErrCallChanged
	}
	// 呼叫等待时模组上会同时挂着两路语音通话，这里必须定向：AT+CHUP 会把
	// 正在通话和等待中的一起断掉，用户只想挂断当前这一路。
	command, err := controlCommand(target.Call, voiceCalls(calls), answer)
	if err != nil {
		return update, err
	}
	return s.execute(opCtx, command, update)
}

// trackedByID 在语音通话快照里按模组通话 ID 取回当前索引。
func trackedByID(calls []TrackedCall, id string) (TrackedCall, bool) {
	for _, call := range calls {
		if id != "" && call.ID == id {
			return call, true
		}
	}
	return TrackedCall{}, false
}

func voiceCalls(calls []TrackedCall) []Call {
	out := make([]Call, 0, len(calls))
	for _, call := range calls {
		out = append(out, call.Call)
	}
	return out
}

func (s *Session) voiceControlCalls() []TrackedCall {
	var voice []TrackedCall
	for _, call := range s.tracker.Calls() {
		if !establishedDataCall(call.Call) {
			voice = append(voice, call)
		}
	}
	return voice
}

func containsCallID(calls []TrackedCall, id string) bool {
	for _, call := range calls {
		if id != "" && call.ID == id {
			return true
		}
	}
	return false
}

func (s *Session) execute(ctx context.Context, command string, update Update) (Update, error) {
	return s.executeCommand(ctx, sessionCommand{text: command, timeout: callCommandTimeout}, update)
}

type sessionCommand struct {
	text    string
	timeout time.Duration
}

func (s *Session) executeCommand(ctx context.Context, command sessionCommand, update Update) (Update, error) {
	update.Attempted = true
	_, err := s.port.ExecuteATContext(ctx, command.text, command.timeout)
	update.Accepted = err == nil
	// Even a timed-out dial may have created a call; wake a fresh poll, never
	// redial automatically or manufacture an ended/connected state here.
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return update, err
}
