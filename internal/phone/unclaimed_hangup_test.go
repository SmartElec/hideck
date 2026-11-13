package phone

import (
	"context"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
)

// 呼叫等待里的第二路来电没有媒体会话，也就没有租约。挂掉第一通之后，
// 它必须仍然能被任何浏览器挂断，否则只能干看着它一直振铃。
func TestHangupUnclaimedIncomingCall(t *testing.T) {
	gateway := newFakeVoiceGateway()
	service := newPhoneTestService(t, gateway, newMemoryCallStore(), time.Second)
	gateway.emitIncoming(voicehost.IncomingCall{
		DeviceID: "dev-1", CallID: "incoming-1", Caller: "+15550001",
		OfferSDP: testPlainSDP, ReceivedAt: time.Now(),
	})
	waitForActiveCall(t, service, "incoming-1")

	if err := service.Hangup(context.Background(), "admin", "incoming-1", ""); err != nil {
		t.Fatalf("未接管的来电必须能挂断: %v", err)
	}
	select {
	case id := <-gateway.hangupCalls:
		if id != "incoming-1" {
			t.Fatalf("hangup target = %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("没有下发挂断")
	}
}

// 已经被别的浏览器接管的通话不能靠空租约抢走。
func TestHangupClaimedCallStillRequiresItsLease(t *testing.T) {
	service := &Service{calls: map[string]*activeCall{
		"call-1": {
			owner: "admin", lease: "lease-1",
			view: CallView{CallID: "call-1", DeviceID: "dev-1", Direction: "inbound", Status: StatusWaiting},
		},
	}}
	if err := service.Hangup(context.Background(), "admin", "call-1", ""); err == nil {
		t.Fatal("已接管的通话被空租约挂断了")
	}
}

// 拒接不需要媒体：等待中的来电与当前通话共用同一个 WebRTC 会话，
// 旧实现会先预留媒体，直接撞上 media session already has an active call。
func TestRejectWaitingCallNeedsNoMedia(t *testing.T) {
	gateway := newFakeVoiceGateway()
	service := newPhoneTestService(t, gateway, newMemoryCallStore(), time.Second)
	service.calls["waiting-1"] = &activeCall{
		view: CallView{
			CallID: "waiting-1", DeviceID: "dev-1", Direction: "inbound",
			Peer: "+15550002", Status: StatusWaiting,
		},
		terminalDone: make(chan struct{}), finalizedDone: make(chan struct{}),
	}
	if err := service.Reject(ControlRequest{
		Owner: "admin", CallID: "waiting-1", MediaID: "", Lease: "",
	}); err != nil {
		t.Fatalf("拒接等待中的来电不该要求媒体租约: %v", err)
	}
	select {
	case request := <-gateway.rejectCalls:
		if request.CallID != "waiting-1" || request.StatusCode != 486 {
			t.Fatalf("reject request = %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("没有下发拒接")
	}
}

func waitForActiveCall(t *testing.T, service *Service, callID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, call := range service.Active("") {
			if call.CallID == callID {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("通话 %s 没有出现在活动列表里", callID)
}
