package volte

import (
	"context"
	"testing"
)

// newATHandoffTest 复刻现场：设备只有 AT 后端，VoLTE 已降级，用户要把
// 通话权交给模组直拨。这类设备没有 QMI，交接不能沿用 QMI 的通话查询。
func newATHandoffTest(t *testing.T) (*Controller, *FakeModem) {
	t.Helper()
	host := newATFallbackHost()
	ctl := NewControllerWithBackup(host, t.TempDir())
	if err := ctl.Enable(context.Background(), "wwan1"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctl.Disable("wwan1") })
	if !ctl.atFallback("wwan1") {
		t.Fatal("测试前提：设备应处于 AT 降级")
	}
	return ctl, host
}

func TestHandoffWithoutQMILandsOnCLCC(t *testing.T) {
	ctl, host := newATHandoffTest(t)
	// 挂断前有一条进行中的语音通话：交接必须把它挂掉再交出去。
	host.setCLCC(`+CLCC: 1,0,0,0,0,"10010",129`)

	if err := ctl.DisableForHandoff(context.Background(), "wwan1"); err != nil {
		t.Fatalf("没有 QMI 时交接应改用 CLCC，不该报 native_volte_stop_failed: %v", err)
	}
	if ctl.atVoiceSession("wwan1") != nil {
		t.Fatal("交接后 AT 会话（含 CLCC 轮询）必须停掉")
	}
	if phase := ctl.Status("wwan1").Phase; phase != PhaseIdle {
		t.Fatalf("交接后 phase=%q，want idle", phase)
	}
}

func TestHandoffWithoutQMIRejectsSurvivingCall(t *testing.T) {
	ctl, host := newATHandoffTest(t)
	host.setCLCC(`+CLCC: 1,0,0,0,0,"10010",129`)
	// 模组接受了 CHUP 但网络侧还没释放：与 QMI 版本一样，拒绝交接。
	host.CHUPKeepsCall = true

	if err := ctl.DisableForHandoff(context.Background(), "wwan1"); err == nil {
		t.Fatal("通话未确认结束时不能交出设备")
	}
	if ctl.atVoiceSession("wwan1") == nil {
		t.Fatal("拒绝交接时必须保留 AT 会话，否则通话会失去归属")
	}

	host.CHUPKeepsCall = false
	host.setCLCC()
	if err := ctl.DisableForHandoff(context.Background(), "wwan1"); err != nil {
		t.Fatalf("通话结束后重试交接: %v", err)
	}
	if ctl.atVoiceSession("wwan1") != nil {
		t.Fatal("重试成功后 AT 会话必须清掉")
	}
}

func TestHandoffWithoutQMIIgnoresDataCall(t *testing.T) {
	ctl, host := newATHandoffTest(t)
	// mode=1 的已建立数据呼叫不该挡住交接，也不该被当成语音挂断。
	host.setCLCC(`+CLCC: 2,0,0,1,0,"",128`)

	if err := ctl.DisableForHandoff(context.Background(), "wwan1"); err != nil {
		t.Fatalf("数据呼叫不该阻塞语音交接: %v", err)
	}
	if ctl.atVoiceSession("wwan1") != nil {
		t.Fatal("交接后 AT 会话必须清掉")
	}
}
