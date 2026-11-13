package volte

import (
	"context"
	"testing"
	"time"
)

// AT 降级链路空闲时 2 秒才轮询一次 CLCC。对方挂断后模组会先吐 NO CARRIER，
// 收到 URC 必须立刻补一次快照，否则界面要等一整个轮询周期才结束通话。
func TestATFallbackPollsImmediatelyOnVoiceURC(t *testing.T) {
	host := newATFallbackHost()
	ctl := NewControllerWithBackup(host, t.TempDir())
	if err := ctl.Enable(context.Background(), "wwan1"); err != nil {
		t.Fatal(err)
	}
	defer ctl.Disable("wwan1")

	// 等空闲轮询至少跑过一轮，之后到下一次定时轮询最多还有 2 秒。
	waitUntil(t, 2*time.Second, func() bool { return host.CountCommand("AT+CLCC") > 0 })
	base := host.CountCommand("AT+CLCC")

	host.WakeVoice()
	waitUntil(t, time.Second, func() bool { return host.CountCommand("AT+CLCC") > base })

	// 轮询协程退出时必须退订，否则模组每次重启都会攒下一个孤儿订阅。
	ctl.Disable("wwan1")
	waitUntil(t, 5*time.Second, func() bool { return host.VoiceWakeUnsubscribes() > 0 })
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%v 内条件没有满足", timeout)
}
