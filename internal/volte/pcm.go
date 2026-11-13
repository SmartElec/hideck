package volte

import (
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/yibaiba/hideck/internal/phone"
	"github.com/yibaiba/hideck/pkg/logger"
)

const (
	pcmuPayloadType   = 0
	pcmuClockRate     = 8000
	pcmuFrameSamples  = 160
	pcmuFrameDuration = 20 * time.Millisecond
)

type PCMPort interface {
	ReadFrame() ([]int16, error)
	WriteFrame([]int16) error
	Close() error
}

type pcmDiagnostics struct {
	XRUN        uint64
	Recovered   uint64
	Unrecovered uint64
}

type pcmDiagnosticsProvider interface {
	pcmDiagnostics() pcmDiagnostics
}

func diagnosticsForPCM(pcm PCMPort) pcmDiagnostics {
	provider, ok := pcm.(pcmDiagnosticsProvider)
	if !ok {
		return pcmDiagnostics{}
	}
	return provider.pcmDiagnostics()
}

type PCMBridge struct {
	conn       net.PacketConn
	mediaMu    sync.Mutex
	remote     net.Addr
	generation uint64
	buffer     *adaptivePCMBuffer
	pcm        PCMPort
	listenOnly bool
	closed     chan struct{}
	closeOnce  sync.Once
	workers    sync.WaitGroup
	seq        uint16
	ts         uint32
	toPCM      atomic.Uint64
	fromPCM    atomic.Uint64
	silent     atomic.Uint64
	lost       atomic.Uint64
	overflow   atomic.Uint64
	jitterMu   sync.Mutex
	jitter     pcmJitterDiagnostics

	// 模组侧音频能量：对方挂断后网络会停掉回铃音，PCM 里只剩静音，而 EC20F
	// 可能还把通话挂在 alerting 上三十多秒。听不到回铃音才是"对端已经不在
	// 了"的最早证据，所以单独记最后一次有声帧和它的大小。
	heard  atomic.Bool
	loudAt atomic.Int64 // UnixNano
	level  atomic.Int64 // 最近一帧的 RMS
	// loud 累计有声帧数：现场只能看到声音断了，用它区分"模组不再出声"和"我们没发"。
	loud atomic.Uint64
	// dump 是排查"回铃音/提示音只响半句"用的原始下行 PCM 录音。现场反馈的
	// 断点只能靠波形定性：数字静音说明模组上游就没有内容，有底噪说明是编码
	// 或增益问题。开关是文件而不是环境变量，容器里不需要重启就能开关。
	dump     *os.File
	dumpPath string
}

// ringbackRMSThreshold 是判定"这一帧有声音"的 RMS 门限。8kHz PCMU 的静音帧
// 基本是 0，回铃音通常上千；300 既不会漏掉偏轻的回铃音，也不会被底噪触发。
const ringbackRMSThreshold = 300

func frameRMS(samples []int16) int64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, sample := range samples {
		value := float64(sample)
		sum += value * value
	}
	return int64(math.Sqrt(sum / float64(len(samples))))
}

// Ringback 报告模组侧是否送出过声音、最后一次有声的时刻和最近一帧的 RMS。
// heard 为 false 时说明整段通话模组都没送来过音频，此时不能用静音判断。
func (b *PCMBridge) Ringback() (heard bool, lastLoud time.Time, rms int64) {
	rms = b.level.Load()
	if !b.heard.Load() {
		return false, time.Time{}, rms
	}
	return true, time.Unix(0, b.loudAt.Load()), rms
}

// LoudFrames 返回模组侧累计送来的有声帧数。配合 Stats 的 fromPCM 增量，能区分
// "声卡还在出帧但已经没声音"和"连帧都停了"。
func (b *PCMBridge) LoudFrames() uint64 { return b.loud.Load() }

type pcmRTPArrival struct {
	packet     *rtp.Packet
	remote     net.Addr
	received   time.Time
	generation uint64
}

type pcmWriteLogState struct {
	overflowLogged bool
	lastOverflow   time.Time
}

func NewPCMBridge(conn net.PacketConn, remote net.Addr, pcm PCMPort, listenOnly bool) *PCMBridge {
	b := &PCMBridge{conn: conn, remote: remote, pcm: pcm, listenOnly: listenOnly, closed: make(chan struct{}), buffer: newAdaptivePCMBuffer()}
	b.dump, b.dumpPath = openPCMDump()
	b.workers.Add(2)
	go b.downlink()
	go b.uplink()
	return b
}

// pcmDumpSwitch 是打开原始下行 PCM 录音的开关文件名。放在日志目录里，宿主机
// touch/rm 即可开关，下一通电话生效，不需要重启容器。
const pcmDumpSwitch = "pcm-dump.on"

// openPCMDump 在开关文件存在时创建一个 8kHz/16bit/单声道的裸 PCM 文件。
// 录的是"我们刚从模组声卡读到的东西"，因此能把"模组没送出声音"和"送出来了
// 但我们没转发"彻底分开。
func openPCMDump() (*os.File, string) {
	dir := os.Getenv("HIDECK_LOG_DIR")
	if dir != "" {
		return dumpInto([]string{dir})
	}
	// 容器里日志目录挂在 /app/logs，宿主机是 /opt/hideck/logs；两种都试一遍，
	// 免得部署形态一变就静默失效。
	return dumpInto([]string{"/app/logs", "/opt/hideck/logs", "logs"})
}

func dumpInto(dirs []string) (*os.File, string) {
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, pcmDumpSwitch)); err != nil {
			continue
		}
		path := filepath.Join(dir, fmt.Sprintf("pcm-%s.raw", time.Now().Format("150405")))
		f, err := os.Create(path)
		if err != nil {
			logger.Warn("VoLTE 原始音频录制开启失败", "path", path, "err", err)
			return nil, ""
		}
		logger.Info("VoLTE 开始录制模组下行音频", "path", path)
		return f, path
	}
	return nil, ""
}

func (b *PCMBridge) writeDump(samples []int16) {
	if b.dump == nil {
		return
	}
	buf := make([]byte, len(samples)*2)
	for i, v := range samples {
		buf[i*2] = byte(v)
		buf[i*2+1] = byte(v >> 8)
	}
	_, _ = b.dump.Write(buf)
}

func (b *PCMBridge) Stats() (toPCM, fromPCM, silent, lost, overflow uint64) {
	return b.toPCM.Load(), b.fromPCM.Load(), b.silent.Load(), b.lost.Load(), b.overflow.Load()
}

func (b *PCMBridge) Close() error {
	var err error
	b.closeOnce.Do(func() {
		close(b.closed)
		if b.conn != nil {
			err = b.conn.Close()
		}
		if b.pcm != nil {
			_ = b.pcm.Close()
		}
		b.workers.Wait()
		if b.dump != nil {
			_ = b.dump.Close()
			logger.Info("VoLTE 原始音频录制已结束", "path", b.dumpPath, "from_modem", b.fromPCM.Load(), "loud", b.loud.Load())
		}
		diagnostics := diagnosticsForPCM(b.pcm)
		jitter := b.jitterStats()
		// 挂断即打模组侧统计：toPCM/overflow 才是"音频是否真的进了模组"的
		// 证据，phone 包的 to_modem 只是回环 UDP 写成功计数，不等于送达模组。
		logger.Info("VoLTE 模组音频统计",
			"to_modem", b.toPCM.Load(), "from_modem", b.fromPCM.Load(),
			"silent", b.silent.Load(), "lost", b.lost.Load(), "overflow", b.overflow.Load(),
			"alsa_xrun", diagnostics.XRUN, "alsa_recovered", diagnostics.Recovered,
			"alsa_unrecovered", diagnostics.Unrecovered,
			"jitter_target_ms", jitter.TargetFrames*uint64(pcmuFrameDuration/time.Millisecond),
			"jitter_max_depth", jitter.MaxDepth,
			"jitter_us", jitter.JitterMicros, "jitter_received", jitter.Received,
			"jitter_late", jitter.Late, "jitter_duplicate", jitter.Duplicate,
			"jitter_concealed", jitter.Concealed, "jitter_trimmed", jitter.Trimmed,
			"jitter_rebuffers", jitter.Rebuffers, "jitter_restarts", jitter.Restarts)
	})
	return err
}

func (b *PCMBridge) downlink() {
	defer b.workers.Done()
	arrivals := make(chan pcmRTPArrival, pcmJitterMaxFrames*2)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		b.readDownlink(arrivals)
	}()
	defer func() { <-readerDone }()
	ticker := time.NewTicker(pcmuFrameDuration)
	defer ticker.Stop()
	defer b.saveJitterStats()
	writeState := &pcmWriteLogState{}
	for {
		select {
		case <-b.closed:
			return
		case arrival, ok := <-arrivals:
			if !ok {
				return
			}
			b.queueDownlink(arrival)
		case <-ticker.C:
			b.playBufferedDownlink(writeState)
		}
	}
}

func (b *PCMBridge) readDownlink(destination chan<- pcmRTPArrival) {
	defer close(destination)
	buf := make([]byte, 2048)
	for {
		if b.conn == nil {
			return
		}
		_ = b.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, addr, err := b.conn.ReadFrom(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		packet, ok := parsePCMURTP(buf[:n])
		if !ok {
			b.lost.Add(1)
			continue
		}
		arrival, accepted := b.acceptArrival(pcmRTPArrival{packet: packet, remote: addr, received: time.Now()})
		if !accepted {
			continue
		}
		select {
		case destination <- arrival:
		case <-b.closed:
			return
		default:
			b.lost.Add(1)
		}
	}
}

func (b *PCMBridge) playDownlink(packet *rtp.Packet, kind pcmPlayoutKind, state *pcmWriteLogState) {
	if kind == pcmPlayoutWaiting || b.pcm == nil {
		return
	}
	samples := make([]int16, pcmuFrameSamples)
	if packet != nil {
		copy(samples, phone.DecodePCMU(packet.Payload))
	} else if kind == pcmPlayoutGap {
		b.lost.Add(1)
	}
	if err := b.pcm.WriteFrame(samples); err != nil {
		b.reportPCMWriteFailure(err, state)
		return
	}
	if kind == pcmPlayoutAudio {
		b.toPCM.Add(1)
	}
}

func (b *PCMBridge) reportPCMWriteFailure(err error, state *pcmWriteLogState) {
	b.overflow.Add(1)
	if state.overflowLogged && time.Since(state.lastOverflow) < 5*time.Second {
		return
	}
	state.overflowLogged, state.lastOverflow = true, time.Now()
	diagnostics := diagnosticsForPCM(b.pcm)
	logger.Warn("VoLTE 上行音频写入模组失败",
		"to_modem", b.toPCM.Load(), "overflow", b.overflow.Load(),
		"alsa_xrun", diagnostics.XRUN, "alsa_recovered", diagnostics.Recovered,
		"alsa_unrecovered", diagnostics.Unrecovered, "err", err)
}

func (b *PCMBridge) setJitterStats(stats pcmJitterDiagnostics) {
	b.jitterMu.Lock()
	b.jitter = stats
	b.jitterMu.Unlock()
}

func (b *PCMBridge) jitterStats() pcmJitterDiagnostics {
	b.jitterMu.Lock()
	defer b.jitterMu.Unlock()
	return b.jitter
}

func (b *PCMBridge) uplink() {
	defer b.workers.Done()
	ticker := time.NewTicker(pcmuFrameDuration)
	defer ticker.Stop()
	for {
		select {
		case <-b.closed:
			return
		case <-ticker.C:
			remote, generation := b.mediaEndpoint()
			if b.conn == nil || remote == nil {
				continue
			}
			samples := make([]int16, pcmuFrameSamples)
			if b.listenOnly || b.pcm == nil {
				b.silent.Add(1)
			} else {
				frame, err := b.pcm.ReadFrame()
				if err != nil {
					b.lost.Add(1)
				} else {
					copy(samples, frame)
					b.fromPCM.Add(1)
					b.writeDump(samples)
					if rms := frameRMS(samples); rms >= ringbackRMSThreshold {
						b.level.Store(rms)
						b.loudAt.Store(time.Now().UnixNano())
						b.heard.Store(true)
						b.loud.Add(1)
					}
				}
			}
			payload := phone.EncodePCMU(samples)
			pkt := encodePCMURTP(b.seq, b.ts, payload)
			b.seq++
			b.ts += pcmuFrameSamples
			if err := b.writeUplink(pkt, generation); err != nil {
				return
			}
		}
	}
}
