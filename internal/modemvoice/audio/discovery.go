// Package audio implements the hardware and process adapters for modem-direct
// audio. Discovery alone never authorizes opening a QMI-sensitive USB device.
package audio

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type Endpoint struct {
	USBPath string
	Card    int
	Device  int
}

func (e Endpoint) ALSADevice() string { return fmt.Sprintf("hw:%d,%d", e.Card, e.Device) }

var pcmNode = regexp.MustCompile(`^pcmC([0-9]+)D([0-9]+)([pc])$`)

// usbDevicesRoot 是 sysfs 的 USB 设备树根，测试里可替换。
var usbDevicesRoot = "/sys/bus/usb/devices"

// ResolveSoleUSBPCM 在 USBPath 不可信时的兜底：整棵 USB 树上只有一个设备
// 同时具备采集与播放 PCM 才认领。多声卡机器继续报错——认错卡会把通话音频
// 串到别的设备上，比没有音频更难排查。
func ResolveSoleUSBPCM() (Endpoint, error) {
	entries, err := os.ReadDir(usbDevicesRoot)
	if err != nil {
		return Endpoint{}, fmt.Errorf("audio: read USB bus: %w", err)
	}
	pairs := make(map[Endpoint]uint8)
	for _, entry := range entries {
		name := entry.Name()
		if strings.Contains(name, ":") {
			// 接口目录由所属设备目录统一遍历，避免同一张卡算两遍。
			continue
		}
		root, err := filepath.EvalSymlinks(filepath.Join(usbDevicesRoot, name))
		if err != nil {
			continue
		}
		ifaces, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, iface := range ifaces {
			if !strings.HasPrefix(iface.Name(), name+":") {
				continue
			}
			// 单个接口读不动（权限/竞态）不该让整轮发现失败。
			_ = collectInterfacePCM(root, iface.Name(), pairs)
		}
	}
	var found []Endpoint
	for endpoint, directions := range pairs {
		if directions == duplexDirections {
			found = append(found, endpoint)
		}
	}
	if len(found) != 1 {
		return Endpoint{}, fmt.Errorf("audio: found %d bidirectional USB PCM devices; require exactly one", len(found))
	}
	return found[0], nil
}

const (
	playbackDirection uint8 = 1 << iota
	captureDirection
	duplexDirections = playbackDirection | captureDirection
)

// ResolveUSBPCM discovers paired capture/playback nodes afresh at each call.
// Card numbers are transient and must not be persisted or selected by card name.
func ResolveUSBPCM(usbPath string) (Endpoint, error) {
	root, err := filepath.EvalSymlinks(usbPath)
	if err != nil {
		return Endpoint{}, fmt.Errorf("audio: resolve USB device: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return Endpoint{}, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return Endpoint{}, fmt.Errorf("audio: read USB interfaces: %w", err)
	}
	pairs := make(map[Endpoint]uint8)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), filepath.Base(root)+":") {
			continue
		}
		if err := collectInterfacePCM(root, entry.Name(), pairs); err != nil {
			return Endpoint{}, err
		}
	}
	var found []Endpoint
	for endpoint, directions := range pairs {
		if directions == duplexDirections {
			found = append(found, endpoint)
		}
	}
	if len(found) != 1 {
		return Endpoint{}, fmt.Errorf("audio: USB device has %d bidirectional PCM endpoints; require exactly one", len(found))
	}
	return found[0], nil
}

func collectInterfacePCM(root, name string, pairs map[Endpoint]uint8) error {
	sound := filepath.Join(root, name, "sound")
	cards, err := os.ReadDir(sound)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("audio: read sound cards: %w", err)
	}
	for _, card := range cards {
		if !strings.HasPrefix(card.Name(), "card") {
			continue
		}
		path, err := filepath.EvalSymlinks(filepath.Join(sound, card.Name()))
		if err != nil {
			return err
		}
		if !strings.HasPrefix(path, root+string(filepath.Separator)) {
			return fmt.Errorf("audio: sound card belongs to another USB device")
		}
		if err := collectCardPCM(path, root, pairs); err != nil {
			return err
		}
	}
	return nil
}

func collectCardPCM(path, root string, pairs map[Endpoint]uint8) error {
	nodes, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("audio: read PCM endpoints: %w", err)
	}
	for _, node := range nodes {
		parts := pcmNode.FindStringSubmatch(node.Name())
		if parts == nil {
			continue
		}
		card, cardErr := strconv.Atoi(parts[1])
		device, devErr := strconv.Atoi(parts[2])
		if cardErr != nil || devErr != nil {
			return fmt.Errorf("audio: invalid PCM node number")
		}
		if filepath.Base(path) != "card"+parts[1] {
			return fmt.Errorf("audio: PCM node does not match its sound card")
		}
		endpoint := Endpoint{USBPath: root, Card: card, Device: device}
		if parts[3] == "p" {
			pairs[endpoint] |= playbackDirection
		} else {
			pairs[endpoint] |= captureDirection
		}
	}
	return nil
}
