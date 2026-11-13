package modemvoice

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// activityExecutor 同时应答 AT+CLCC 与 AT+CPAS，用来复刻"CLCC 残留、CPAS 已空闲"。
type activityExecutor struct {
	clcc     string
	clccErr  error
	cpas     string
	cpasErr  error
	commands []string
}

func (e *activityExecutor) ExecuteATContext(_ context.Context, command string, _ time.Duration) (string, error) {
	e.commands = append(e.commands, command)
	switch command {
	case "AT+CLCC":
		return e.clcc, e.clccErr
	case "AT+CPAS":
		return e.cpas, e.cpasErr
	}
	return "", nil
}

func (e *activityExecutor) probedCPAS() bool {
	for _, command := range e.commands {
		if command == "AT+CPAS" {
			return true
		}
	}
	return false
}

// withCPASGrace 把宽限压到 0/放开，测完恢复默认值。
func withCPASGrace(t *testing.T, grace time.Duration) {
	t.Helper()
	previous := cpasGrace
	cpasGrace = grace
	t.Cleanup(func() { cpasGrace = previous })
}

const ringingCLCC = "+CLCC: 1,0,3,0,0,\"10086\",129"

// 对方挂断后 EC20F 偶尔把通话留在 CLCC 里，只有 CPAS 回到 ready。
// 不处理的话轮询永远等不到"通话消失"，界面就一直停在响铃。
func TestCallsDropsCallThatCLCCStillListsWhenCPASIsIdle(t *testing.T) {
	withCPASGrace(t, 0)
	executor := &activityExecutor{clcc: ringingCLCC, cpas: "+CPAS: 0\r\nOK"}
	client, _ := NewClient(executor)

	first, err := client.CallsVerified(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("第一次观测就丢弃了通话: %+v", first)
	}
	second, err := client.CallsVerified(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("CPAS 连续两轮空闲后仍未释放残留通话: %+v", second)
	}
}

func TestCallsKeepsCallWhileCPASReportsInCall(t *testing.T) {
	withCPASGrace(t, 0)
	executor := &activityExecutor{clcc: ringingCLCC, cpas: "+CPAS: 4\r\nOK"}
	client, _ := NewClient(executor)
	for i := 0; i < 3; i++ {
		calls, err := client.CallsVerified(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) != 1 {
			t.Fatalf("第 %d 轮把真在通话的呼叫丢弃了: %+v", i+1, calls)
		}
	}
}

func TestCallsKeepsFreshCallBeforeGraceElapses(t *testing.T) {
	withCPASGrace(t, time.Minute)
	executor := &activityExecutor{clcc: ringingCLCC, cpas: "+CPAS: 0\r\nOK"}
	client, _ := NewClient(executor)
	for i := 0; i < 3; i++ {
		calls, err := client.CallsVerified(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) != 1 {
			t.Fatalf("宽限期内不该结束刚下发的呼叫: %+v", calls)
		}
	}
}

func TestCallsIgnoresCPASFailure(t *testing.T) {
	withCPASGrace(t, 0)
	executor := &activityExecutor{clcc: ringingCLCC, cpasErr: errors.New("AT+CPAS timeout")}
	client, _ := NewClient(executor)
	for i := 0; i < 3; i++ {
		calls, err := client.CallsVerified(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) != 1 {
			t.Fatalf("CPAS 不可用时不能凭它结束通话: %+v", calls)
		}
	}
}

func TestCallsIgnoresUnparsableCPAS(t *testing.T) {
	withCPASGrace(t, 0)
	executor := &activityExecutor{clcc: ringingCLCC, cpas: "ERROR"}
	client, _ := NewClient(executor)
	for i := 0; i < 3; i++ {
		calls, err := client.CallsVerified(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) != 1 {
			t.Fatalf("CPAS 解析失败时不能当成空闲: %+v", calls)
		}
	}
}

// 数据/传真呼叫不进语音状态机，也不该触发 CPAS 探测。
func TestCallsDoesNotProbeCPASWithoutVoiceCalls(t *testing.T) {
	withCPASGrace(t, 0)
	executor := &activityExecutor{clcc: "+CLCC: 2,1,0,1,0,\"\",128", cpas: "+CPAS: 0\r\nOK"}
	client, _ := NewClient(executor)
	calls, err := client.CallsVerified(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Mode != 1 {
		t.Fatalf("数据呼叫被吞掉了: %+v", calls)
	}
	if executor.probedCPAS() {
		t.Fatal("没有语音通话时不该额外打 AT+CPAS")
	}
}

// 残留判定只针对语音通话：同一条快照里的数据呼叫必须原样返回。
func TestCallsKeepsDataCallWhileDroppingStaleVoiceCall(t *testing.T) {
	withCPASGrace(t, 0)
	executor := &activityExecutor{
		clcc: strings.Join([]string{"+CLCC: 2,1,0,1,0,\"\",128", ringingCLCC}, "\n"),
		cpas: "+CPAS: 0\r\nOK",
	}
	client, _ := NewClient(executor)
	if _, err := client.CallsVerified(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls, err := client.CallsVerified(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Mode != 1 {
		t.Fatalf("残留语音通话释放后应只剩数据呼叫: %+v", calls)
	}
}

// 残留解除后（CPAS 又报通话中）观测记录必须清零，避免下一次通话被立刻判定结束。
func TestCallsResetsStrikesWhenCPASReportsInCall(t *testing.T) {
	withCPASGrace(t, 0)
	executor := &activityExecutor{clcc: ringingCLCC, cpas: "+CPAS: 0\r\nOK"}
	client, _ := NewClient(executor)
	if _, err := client.CallsVerified(context.Background()); err != nil {
		t.Fatal(err)
	}
	executor.cpas = "+CPAS: 4\r\nOK"
	if calls, err := client.CallsVerified(context.Background()); err != nil || len(calls) != 1 {
		t.Fatalf("calls=%+v err=%v", calls, err)
	}
	executor.cpas = "+CPAS: 0\r\nOK"
	calls, err := client.CallsVerified(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("清零后的第一次观测就释放了通话: %+v", calls)
	}
}
