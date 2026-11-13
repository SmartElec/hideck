package device

import "testing"

// 现场机器的 /proc/asound/cards：平台声卡 0/1 + 模组 USB 声卡 2。
const procAsoundCardsSample = ` 0 [rockchiprk809co]: simple-card - rockchip,rk809-codec
                      rockchip,rk809-codec
 1 [rockchiphdmin  ]: simple-card - rockchip,hdmin
                      rockchip,hdmin
 2 [Android        ]: USB-Audio - Android
                      Android Android at usb-xhci-hcd.0.auto-1, high speed
`

func TestParseSoleUSBAudioCard(t *testing.T) {
	if got := parseSoleUSBAudioCard(procAsoundCardsSample); got != 2 {
		t.Fatalf("card=%d want 2", got)
	}
}

func TestParseSoleUSBAudioCardRefusesAmbiguous(t *testing.T) {
	// 两张 USB 声卡时猜错会把音频串到别的设备，宁可不选。
	cards := ` 2 [Android  ]: USB-Audio - Android
 3 [Headset  ]: USB-Audio - USB Headset
`
	if got := parseSoleUSBAudioCard(cards); got != -1 {
		t.Fatalf("多张 USB 声卡时应当放弃，got %d", got)
	}
}

func TestParseSoleUSBAudioCardWithoutUSB(t *testing.T) {
	if got := parseSoleUSBAudioCard(" 0 [rockchiprk809co]: simple-card - rockchip,rk809-codec\n"); got != -1 {
		t.Fatalf("没有 USB 声卡应返回 -1，got %d", got)
	}
}
