package volte

import (
	"context"
	"errors"
	"testing"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/yibaiba/hideck/internal/modemvoice"
)

// newATFallbackHost 复刻 .99 上的现场：设备只有 AT 后端（ECM，无 cdc-wdm0），
// 模块自身 IMS 已开、LTE 已附着，QMI 一侧全部报"没有 QMI"。
func newATFallbackHost() *FakeModem {
	host := newFakeModem()
	host.IMS, host.VoLTE = 1, 1
	host.USB[len(host.USB)-1] = "1"
	host.QMICore = false
	host.EnsureErr = errors.New("设备 wwan1 没有 QMI")
	return host
}

func TestControllerFallsBackToATWhenDeviceHasNoQMI(t *testing.T) {
	host := newATFallbackHost()
	ctl := NewControllerWithBackup(host, t.TempDir())
	defer ctl.Disable("wwan1")

	if err := ctl.Enable(context.Background(), "wwan1"); err != nil {
		t.Fatalf("没有 QMI 时应降级到 AT，而不是判死: %v", err)
	}
	st := ctl.Status("wwan1")
	if !st.ATFallback || !st.QMIIMSUnavailable {
		t.Fatalf("status must expose the AT fallback: %+v", st)
	}
	// PDN / IMS / Voice 三个阶段必须由 AT 探测补齐，UI 才会放行拨号。
	if !st.IMSPDNActive || !st.IMSRegistered || !st.VoiceIndicationsReady {
		t.Fatalf("AT 探测必须补齐 PDN/IMS/Voice: %+v", st)
	}
	if st.Phase != PhaseRegistered || !st.Ready() {
		t.Fatalf("phase=%q ready=%v", st.Phase, st.Ready())
	}
	if host.VoiceEnsureCount != 0 || host.SetNative != 0 {
		t.Fatalf("降级路径不该再碰 QMI: voiceEnsure=%d setNative=%d", host.VoiceEnsureCount, host.SetNative)
	}
	if ctl.atVoiceSession("wwan1") == nil {
		t.Fatal("AT 降级会话未启动")
	}
}

func TestControllerWithoutLTEFailsBeforeClaimingATFallback(t *testing.T) {
	host := newATFallbackHost()
	host.CEREG = 2
	ctl := NewControllerWithBackup(host, t.TempDir())
	defer ctl.Disable("wwan1")

	// 降级只换掉"用 QMI 判 IMS 就绪"这一步，LTE 附着仍然是硬门槛。
	if err := ctl.Enable(context.Background(), "wwan1"); err == nil {
		t.Fatal("LTE 未附着时不能对外声称 IMS 就绪")
	}
	st := ctl.Status("wwan1")
	if st.IMSRegistered || st.Ready() || st.Phase == PhaseRegistered {
		t.Fatalf("status %+v", st)
	}
	if st.ATFallback {
		t.Fatal("LTE 门控失败不该被记成 AT 降级")
	}
}

func TestControllerATFallbackDialsAndFollowsCLCC(t *testing.T) {
	host := newATFallbackHost()
	ctl := NewControllerWithBackup(host, t.TempDir())
	defer ctl.Disable("wwan1")
	if err := ctl.Enable(context.Background(), "wwan1"); err != nil {
		t.Fatal(err)
	}
	voice := ctl.atVoiceSession("wwan1")
	if voice == nil {
		t.Fatal("AT 降级会话未启动")
	}
	var events []voicehost.CallEvent
	ctl.SubscribeCallEvents(func(event voicehost.CallEvent) { events = append(events, event) })

	snapshot, err := ctl.BeginCall(context.Background(), voicehost.BeginCallRequest{DeviceID: "wwan1", Callee: "10010"})
	if err != nil {
		t.Fatalf("AT 降级下必须能拨号: %v", err)
	}
	host.mu.Lock()
	dial := host.DialAT
	host.mu.Unlock()
	if dial != "ATD10010;" {
		t.Fatalf("降级拨号必须走 ATD<number>;，实际 %q", dial)
	}

	// CLCC 报振铃 → 接通，状态机必须跟着走并复用 BeginCall 的 CallID。
	for _, clcc := range []string{
		`+CLCC: 1,0,3,0,0,"10010",129`,
		`+CLCC: 1,0,0,0,0,"10010",129`,
	} {
		calls, parseErr := modemvoice.ParseCLCC(clcc + "\r\nOK\r\n")
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		host.setCLCC(clcc)
		ctl.applyATCalls(voice, calls)
	}
	if !hasCallEvent(events, "CallRinging", snapshot.CallID) || !hasCallEvent(events, "CallAnswered", snapshot.CallID) {
		t.Fatalf("events %v", eventTypes(events))
	}
	active := ctl.ActiveCall("wwan1")
	if active == nil || active.State != "connected" || active.CallID != snapshot.CallID {
		t.Fatalf("ActiveCall %+v", active)
	}

	if err := ctl.SendCallDTMF("wwan1", snapshot.CallID, "5"); err != nil {
		t.Fatalf("DTMF 必须走 AT+VTS: %v", err)
	}
	host.mu.Lock()
	sawVTS := len(host.dtmfActions) > 0 && host.dtmfActions[len(host.dtmfActions)-1] == `AT+VTS="5",3`
	host.mu.Unlock()
	if !sawVTS {
		t.Fatal("DTMF 没有下发 AT+VTS")
	}

	if err := ctl.HangupCall(context.Background(), "wwan1", snapshot.CallID); err != nil {
		t.Fatalf("挂断失败: %v", err)
	}
	if !hasCallEvent(events, "CallEnded", snapshot.CallID) {
		t.Fatalf("events %v", eventTypes(events))
	}
	if ctl.ActiveCall("wwan1") != nil {
		t.Fatal("挂断后不应残留活动通话")
	}
}

// EC20F 实测 AT+CIREG? 返回 ERROR：此时只能看 ims PDN 是否真的激活，
// 否则 LTE 一附着就谎报 IMS 就绪，UI 会放行一次必然失败的拨号。
func TestATRegistrationWithoutCIREGFallsBackToIMSPDN(t *testing.T) {
	host := newATFallbackHost()
	host.CIREGError = true
	ctl := NewControllerWithBackup(host, t.TempDir())
	defer ctl.Disable("wwan1")

	host.mu.Lock()
	host.imsActive = false
	host.mu.Unlock()
	if reg := ctl.atCheckRegistration("wwan1"); reg.IMSRegistered {
		t.Fatalf("ims PDN 未激活时不能报 IMS 就绪: %+v", reg)
	}

	host.mu.Lock()
	host.imsActive = true
	host.mu.Unlock()
	reg := ctl.atCheckRegistration("wwan1")
	if !reg.IMSRegistered || !reg.IMSPDN || !reg.Known {
		t.Fatalf("ims PDN 激活后应判定 IMS 就绪: %+v", reg)
	}
}

func hasCallEvent(events []voicehost.CallEvent, kind, callID string) bool {
	for _, event := range events {
		if event.Type == kind && event.CallID == callID {
			return true
		}
	}
	return false
}
