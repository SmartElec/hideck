package volte

import (
	"context"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/modemvoice"
)

// 一台设备只能有一个 CLCC 轮询器。会话被重建/交接后，旧协程必须自己退出：
// 多个轮询各自维护一份 atCall，会给同一通电话发出多个 CallID，也会把 AT
// 通道挤到超时，通话状态反而更不可靠。
func TestATVoicePollerExitsAfterSessionHandsOver(t *testing.T) {
	host := newATFallbackHost()
	ctl := NewControllerWithBackup(host, t.TempDir())
	defer ctl.Disable("wwan1")
	if err := ctl.Enable(context.Background(), "wwan1"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	owner := ctl.atVoiceSession("wwan1")
	if owner == nil {
		t.Fatal("AT 降级会话未启动")
	}

	client, err := modemvoice.NewClient(atVoiceATPort{host: host, deviceID: "wwan1"})
	if err != nil {
		t.Fatal(err)
	}
	stray := &atVoice{deviceID: "wwan1", client: client, done: make(chan struct{}), calls: map[int]*atCall{}}
	go ctl.runATVoice(context.Background(), stray)
	select {
	case <-stray.done:
	case <-time.After(3 * time.Second):
		t.Fatal("失去归属的轮询协程没有退出")
	}
	if ctl.atVoiceSession("wwan1") != owner {
		t.Fatal("轮询退出不该动当前会话")
	}
}

// 轮询协程已经退出（AT 通道长时间不可用、会话被停过）时，startATVoice 必须
// 换来一个新的，而不是把一个死会话交回去——那会让 CLCC 轮询再也不恢复。
func TestStartATVoiceReplacesStoppedPoller(t *testing.T) {
	host := newATFallbackHost()
	ctl := NewControllerWithBackup(host, t.TempDir())
	defer ctl.Disable("wwan1")
	if err := ctl.Enable(context.Background(), "wwan1"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	stopped := ctl.atVoiceSession("wwan1")
	if stopped == nil {
		t.Fatal("AT 降级会话未启动")
	}
	ctl.stopATVoice("wwan1")
	// 把已经停掉的会话塞回会话槽，模拟"指针还在、协程已经没了"。
	ctl.mu.Lock()
	ctl.ensureLocked("wwan1").atVoice = stopped
	ctl.mu.Unlock()

	revived := ctl.startATVoice("wwan1")
	if revived == nil {
		t.Fatal("没能重建轮询会话")
	}
	if revived == stopped {
		t.Fatal("把已停止的轮询协程交给了调用方")
	}
}
