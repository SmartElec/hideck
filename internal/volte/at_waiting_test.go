package volte

import (
	"context"
	"strings"
	"testing"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/yibaiba/hideck/internal/modemvoice"
)

// EC20F 的 CLCC 偶尔会少列一路通话。等待中的来电如果被单次缺失判成挂断，
// 下一次快照就会以新的 CallID 重新出现，上层把它当成新来电反复判忙，
// 最后界面上连挂断的入口都没有。
func TestATWaitingCallSurvivesOneMissedSnapshot(t *testing.T) {
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

	active := `+CLCC: 1,0,0,0,0,"10086",129`
	waiting := `+CLCC: 2,1,5,0,0,"15882275310",128`
	apply := func(lines ...string) {
		body := strings.Join(lines, "\r\n") + "\r\nOK\r\n"
		calls, err := modemvoice.ParseCLCC(body)
		if err != nil {
			t.Fatal(err)
		}
		ctl.applyATCalls(voice, calls)
	}

	apply(active, waiting)
	waitingID := waitingCallID(events)
	if waitingID == "" {
		t.Fatalf("等待来电没有上报: %v", eventTypes(events))
	}

	// 第一次漏掉：只是快照抖动，不能当成挂断。
	apply(active)
	if hasCallEvent(events, "CallCanceled", waitingID) {
		t.Fatal("单次缺失就把等待中的来电判成挂断了")
	}

	// 重新出现后计数清零，再来一次单缺仍然不算结束。
	apply(active, waiting)
	apply(active)
	if hasCallEvent(events, "CallCanceled", waitingID) {
		t.Fatal("重新出现后的首次缺失又判成挂断")
	}

	// 连续两次缺失才认定这通电话结束了。
	apply(active)
	if !hasCallEvent(events, "CallCanceled", waitingID) {
		t.Fatalf("连续缺失后应判定结束: %v", eventTypes(events))
	}
	if countCallEvents(events, "CallWaiting") != 1 {
		t.Fatalf("同一路等待来电被重复上报: %v", eventTypes(events))
	}
}

func waitingCallID(events []voicehost.CallEvent) string {
	for _, event := range events {
		if event.Type == "CallWaiting" {
			return event.CallID
		}
	}
	return ""
}

func countCallEvents(events []voicehost.CallEvent, kind string) int {
	count := 0
	for _, event := range events {
		if event.Type == kind {
			count++
		}
	}
	return count
}
