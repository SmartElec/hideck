package modemvoice

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// 呼叫等待时模组上会同时挂着两路语音通话：一路 active，一路 waiting。
const (
	activeVoiceCLCC  = "+CLCC: 1,0,0,0,0,\"10010\",129"
	waitingVoiceCLCC = "+CLCC: 2,1,5,0,0,\"10086\",129"
)

func TestSessionHangupTargetsOnlyTheActiveCallWhenWaiting(t *testing.T) {
	p := newSessionPort()
	s := newTestSession(t, p)
	if _, err := s.Dial(context.Background(), "10010"); err != nil {
		t.Fatal(err)
	}
	p.response = activeVoiceCLCC
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	active := s.voiceControlCalls()
	if len(active) != 1 {
		t.Fatalf("voice snapshot: %+v", active)
	}
	p.response = activeVoiceCLCC + "\n" + waitingVoiceCLCC
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := s.voiceControlCalls()
	if len(calls) != 2 {
		t.Fatalf("want two voice calls, got %+v", calls)
	}
	if _, err := s.Hangup(context.Background(), active[0].ID); err != nil {
		t.Fatal(err)
	}
	// 只有一路通话时才发整段 CHUP；多路必须定向到索引 1。
	if got := p.commands[len(p.commands)-1]; got != "AT+CHLD=11" {
		t.Fatalf("hangup command = %q; want AT+CHLD=11 (commands %v)", got, p.commands)
	}
}

func TestSessionRejectTargetsTheWaitingCall(t *testing.T) {
	p := newSessionPort()
	p.response = activeVoiceCLCC + "\n" + waitingVoiceCLCC
	s := newTestSession(t, p)
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	var waiting TrackedCall
	for _, call := range s.voiceControlCalls() {
		if call.Call.State == Waiting {
			waiting = call
		}
	}
	if waiting.ID == "" {
		t.Fatalf("waiting call missing: %+v", s.voiceControlCalls())
	}
	if _, err := s.Hangup(context.Background(), waiting.ID); err != nil {
		t.Fatal(err)
	}
	// 等待中的那一路只能用 CHLD=0（UDUB）拒掉：CHLD=12 释放的是 active/held，
	// 既挂不掉等待来电，还会把当前通话一起甩掉。
	if got := p.commands[len(p.commands)-1]; got != "AT+CHLD=0" {
		t.Fatalf("reject command = %q; want AT+CHLD=0", got)
	}
}

func TestSessionAnswersWaitingCallByHoldingTheActiveOne(t *testing.T) {
	p := newSessionPort()
	p.response = activeVoiceCLCC + "\n" + waitingVoiceCLCC
	s := newTestSession(t, p)
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	var waiting TrackedCall
	for _, call := range s.voiceControlCalls() {
		if call.Call.State == Waiting {
			waiting = call
		}
	}
	if _, err := s.Answer(context.Background(), waiting.ID); err != nil {
		t.Fatal(err)
	}
	// CHLD=2 保持当前通话并接听等待中的那一路；ATA 在有 active 通话时并不转接。
	if got := p.commands[len(p.commands)-1]; got != "AT+CHLD=2" {
		t.Fatalf("answer command = %q; want AT+CHLD=2", got)
	}
}

func TestClientHangupTargetsCallByIndex(t *testing.T) {
	p := &executorStub{response: activeVoiceCLCC + "\n" + waitingVoiceCLCC}
	c, _ := NewClient(p)
	if err := c.Hangup(context.Background(), Call{Index: 2, Inbound: true, State: Waiting, Number: "10086"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"AT+CLCC", "AT+CHLD=0"}
	if !reflect.DeepEqual(p.commands, want) {
		t.Fatalf("commands = %v; want %v", p.commands, want)
	}
}

func TestClientHangupKeepsChupForSingleCall(t *testing.T) {
	p := &executorStub{response: activeVoiceCLCC}
	c, _ := NewClient(p)
	if err := c.Hangup(context.Background(), Call{Index: 1, State: Active, Number: "10010"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"AT+CLCC", "AT+CHUP"}
	if !reflect.DeepEqual(p.commands, want) {
		t.Fatalf("commands = %v; want %v", p.commands, want)
	}
}

func TestClientRejectsStaleCallIndex(t *testing.T) {
	p := &executorStub{response: activeVoiceCLCC + "\n" + waitingVoiceCLCC}
	c, _ := NewClient(p)
	// 索引存在但号码已经换人：不能把新通话当成旧通话挂断。
	err := c.Hangup(context.Background(), Call{Index: 2, Inbound: true, State: Waiting, Number: "10000"})
	if err == nil {
		t.Fatal("stale call accepted")
	}
	if !reflect.DeepEqual(p.commands, []string{"AT+CLCC"}) {
		t.Fatalf("commands = %v", p.commands)
	}
}

// retryStub 第一次返回半截 CLCC（带 CME ERROR），第二次正常。
type retryStub struct {
	commands []string
	failures int
	err      error
	response string
}

func (s *retryStub) ExecuteATContext(_ context.Context, command string, _ time.Duration) (string, error) {
	s.commands = append(s.commands, command)
	if command != "AT+CLCC" {
		return "", nil
	}
	if s.failures > 0 {
		s.failures--
		return "", s.err
	}
	return s.response, nil
}

func TestClientRetriesPartialCLCCSnapshot(t *testing.T) {
	stub := &retryStub{failures: 1,
		err:      errors.New("设备返回错误: +CLCC: 1,0,0,0,0,\"10010\",129\r\n+CME ERROR: 100"),
		response: activeVoiceCLCC}
	c, _ := NewClient(stub)
	calls, err := c.Calls(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Index != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	if want := []string{"AT+CLCC", "AT+CLCC"}; !reflect.DeepEqual(stub.commands, want) {
		t.Fatalf("commands = %v; want %v", stub.commands, want)
	}
}

func TestClientDoesNotRetryTransportErrors(t *testing.T) {
	stub := &retryStub{failures: 1, err: context.DeadlineExceeded, response: activeVoiceCLCC}
	c, _ := NewClient(stub)
	if _, err := c.Calls(context.Background()); err == nil {
		t.Fatal("transport error ignored")
	}
	if want := []string{"AT+CLCC"}; !reflect.DeepEqual(stub.commands, want) {
		t.Fatalf("commands = %v; want %v", stub.commands, want)
	}
}

// 呼叫等待时挂掉当前通话走 CHLD=1<idx>，但拒掉等待中的那一路不能用它：
// 27.007 里 CHLD=1<idx> 释放的是 active/held，等待来电要用 CHLD=0（UDUB）。
func TestControlCommandRejectsWaitingCallWithoutDroppingTheActiveOne(t *testing.T) {
	active := Call{Index: 1, State: Active, Number: "10086"}
	waiting := Call{Index: 2, State: Waiting, Inbound: true, Number: "15882275310"}
	both := []Call{active, waiting}

	command, err := controlCommand(waiting, both, false)
	if err != nil {
		t.Fatal(err)
	}
	if command != "AT+CHLD=0" {
		t.Fatalf("拒接等待来电的命令 = %q", command)
	}

	command, err = controlCommand(active, both, false)
	if err != nil {
		t.Fatal(err)
	}
	if command != "AT+CHLD=11" {
		t.Fatalf("挂断当前通话的命令 = %q", command)
	}

	// 只剩等待中的那一路时仍然用 CHUP（已验证过的单路路径）。
	command, err = controlCommand(waiting, []Call{waiting}, false)
	if err != nil {
		t.Fatal(err)
	}
	if command != "AT+CHUP" {
		t.Fatalf("单路拒接的命令 = %q", command)
	}
}
