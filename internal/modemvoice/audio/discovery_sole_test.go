package audio

import (
	"path/filepath"
	"testing"
)

// useFakeUSBBus 把 sysfs 根指到临时目录，避免测试碰真机设备树。
func useFakeUSBBus(t *testing.T) string {
	t.Helper()
	bus := t.TempDir()
	restore := usbDevicesRoot
	usbDevicesRoot = bus
	t.Cleanup(func() { usbDevicesRoot = restore })
	return bus
}

func TestResolveSoleUSBPCMClaimsTheOnlyDuplexDevice(t *testing.T) {
	bus := useFakeUSBBus(t)
	addPCM(t, filepath.Join(bus, "1-1"), 2, 0, "pc")

	endpoint, err := ResolveSoleUSBPCM()
	if err != nil {
		t.Fatalf("唯一的双向声卡应被认领: %v", err)
	}
	if endpoint.ALSADevice() != "hw:2,0" {
		t.Fatalf("alsa=%s", endpoint.ALSADevice())
	}
	if endpoint.USBPath == "" {
		t.Fatal("兜底路径也必须带上 USBPath，后续每次开流都要重新解析声卡号")
	}
}

func TestResolveSoleUSBPCMRefusesAmbiguousBus(t *testing.T) {
	bus := useFakeUSBBus(t)
	addPCM(t, filepath.Join(bus, "1-1"), 2, 0, "pc")
	addPCM(t, filepath.Join(bus, "1-2"), 3, 0, "pc")

	if _, err := ResolveSoleUSBPCM(); err == nil {
		t.Fatal("多张 USB 声卡时不能猜：认错卡会把通话音频串到别的设备")
	}
}

func TestResolveSoleUSBPCMIgnoresHalfDuplex(t *testing.T) {
	bus := useFakeUSBBus(t)
	addPCM(t, filepath.Join(bus, "1-1"), 4, 0, "c")

	if _, err := ResolveSoleUSBPCM(); err == nil {
		t.Fatal("只有采集方向的声卡不能当通话端点")
	}
}
