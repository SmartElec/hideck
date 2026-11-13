package phone

import (
	"fmt"
	"net"
	"testing"
	"time"
)

// 出站呼叫在拨号返回后就应该把媒体接到模组侧：否则回铃音、运营商忙音这类
// 早期媒体全部丢在未 attach 的闸门外面，浏览器整段响铃是静音。
func TestOutboundMediaAttachesBeforeAnswer(t *testing.T) {
	modem, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen modem side: %v", err)
	}
	defer modem.Close()
	browser, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen browser side: %v", err)
	}
	defer browser.Close()

	session := &MediaSession{
		ID: "media-early", Lease: "lease", rtpConn: browser,
		closed: make(chan struct{}),
	}
	service := &Service{media: &MediaManager{sessions: map[string]*MediaSession{session.ID: session}}}

	clientSDP := fmt.Sprintf("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio %d RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n",
		modem.LocalAddr().(*net.UDPAddr).Port)

	service.attachEarlyOutboundMedia("call-1", session.ID, clientSDP)

	if _, _, ok := session.endpoint(); !ok {
		t.Fatal("拨号返回后媒体仍未桥接，响铃阶段的声音到不了浏览器")
	}
	if got := service.media.Get(session.ID); got == nil {
		t.Fatal("media session lost")
	}
	if !waitForUDPFrame(t, modem, time.Second) {
		t.Fatal("桥接后没有向模组侧发出 RTP 帧")
	}

	// 接通回调会再调一次 Attach：同一个端点不应重建编解码器、重发 priming 帧。
	service.attachEarlyOutboundMedia("call-1", session.ID, clientSDP)
	if waitForUDPFrame(t, modem, 300*time.Millisecond) {
		t.Fatal("重复 bridge 同一个端点又发了一帧，会打断通话音频")
	}
}

func waitForUDPFrame(t *testing.T, conn *net.UDPConn, timeout time.Duration) bool {
	t.Helper()
	buffer := make([]byte, 2048)
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	_, _, err := conn.ReadFromUDP(buffer)
	return err == nil
}
