package volte

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/yibaiba/hideck/internal/modemvoice"
	"github.com/yibaiba/hideck/pkg/logger"
)

// 模块没有 QMI（ECM/RNDIS 下没有 /dev/cdc-wdm0，或 QMI 服务栈本身故障）时，
// 原生 VoLTE 的 QMI IMSA 门控会把整条链路判死：UI 显示"缺少 IMS"、拨号被拒。
// 但这类模块的 VoLTE 完全可用——IMS 由模块自己注册，拨号走 ATD，语音走 UAC
// 声卡。下面这套实现就是那条降级链路：状态用 AT 探测（QCFG/ims、CGDCONT、
// CEREG），通话用 modemvoice 的 AT 状态机（CLCC 轮询 + ATD/ATA/CHUP）。
const (
	atFallbackPollIdle = 2 * time.Second
	atFallbackPollBusy = 800 * time.Millisecond
	// atRefreshInterval 是 AT 降级下重新探测注册的最小间隔：一次探测要跑
	// CEREG+CIREG+CGDCONT+CGACT 四条命令，太密会和短信/拨号抢 AT 通道。
	atRefreshInterval = 5 * time.Second
	atHangupReconcile = 4 * time.Second
	atPCMVTimeout     = 2 * time.Second
	atDtmfTimeout     = 5 * time.Second
	// EC25/EC21 AT 手册 §12.4：VTS 的时长单位是 100ms。
	atDtmfDurationTenths = 3
)

// atVoiceATPort 把 Host 的 AT 通道适配成 modemvoice.Port。
// 降级路径没有 URC 订阅者，通话状态完全靠 CLCC 轮询。
type atVoiceATPort struct {
	host     Host
	deviceID string
}

func (p atVoiceATPort) ExecuteATContext(ctx context.Context, command string, timeout time.Duration) (string, error) {
	if p.host == nil {
		return "", errors.New("volte: AT transport is not configured")
	}
	type outcome struct {
		response string
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, err := p.host.ExecuteAT(p.deviceID, command, timeout)
		done <- outcome{response: response, err: err}
	}()
	select {
	case result := <-done:
		return result.response, result.err
	case <-ctx.Done():
		// 底层 ExecuteAT 没有 context，只能放弃等待；命令本身仍会在队列里跑完。
		return "", ctx.Err()
	}
}

func (p atVoiceATPort) SubscribeVoiceChanges() (<-chan struct{}, func()) {
	return make(chan struct{}), func() {}
}

func (p atVoiceATPort) WaitATIdle(context.Context) error { return nil }

// atCall 是降级链路自己维护的一通电话。不要在持锁时调用 Controller 的
// patch/emit 系列方法，它们会拿 Controller 锁。
type atCall struct {
	ID        string
	Index     int
	Peer      string
	Direction string
	State     string
	Held      bool
	Start     time.Time
	Rang      bool // 来电已经通过 IncomingCall 上报给上层
	Connected bool
	// ClientSDP 是控制器侧音频端点的 SDP。phone 包在 CallAnswered 时用
	// ActiveCall().ClientSDP 把网页媒体接进来，缺失会被判成"媒体端点不可用"
	// 并直接挂断，所以必须镜像进 voiceSession。
	ClientSDP string
	// Misses 记录 CLCC 连续漏掉这通电话的次数。EC20F 的快照偶尔会少列
	// 一路通话（尤其是 waiting），单次缺失就当挂断会换掉 CallID，上层于是
	// 把同一路来电当成新来电反复判忙。
	Misses int
}

type atVoice struct {
	deviceID string
	client   *modemvoice.Client
	cancel   context.CancelFunc
	done     chan struct{}

	mu      sync.Mutex
	calls   map[int]*atCall
	pending *atCall // 已下发 ATD、CLCC 还没确认的呼出
	seq     uint64
}

func (v *atVoice) busy() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.calls) > 0 || v.pending != nil
}

func (v *atVoice) get(index int) *atCall {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls[index]
}

func (v *atVoice) byID(id string) *atCall {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, call := range v.calls {
		if call.ID == id {
			return call
		}
	}
	return nil
}

// bind 把 CLCC 里的一个 index 绑定到一通电话。呼出会优先复用 pending 里的
// 通话（ATD 已经下发，ID 必须和 BeginCall 返回给上层的一致）。
func (v *atVoice) bind(index int, call modemvoice.Call, now time.Time) (*atCall, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if existing, ok := v.calls[index]; ok {
		return existing, false
	}
	bindPending := v.pending != nil && !call.Inbound
	target := v.pending
	if bindPending {
		v.pending = nil
	} else {
		v.seq++
		target = &atCall{ID: newPersistentCallID(v.deviceID, uint8(index)), Start: now}
	}
	target.Index = index
	if direction := atCallDirection(call); target.Direction == "" {
		target.Direction = direction
	}
	if peer := strings.TrimSpace(call.Number); peer != "" || target.Peer == "" {
		target.Peer = peer
	}
	if target.Start.IsZero() {
		target.Start = now
	}
	v.calls[index] = target
	return target, true
}

func (v *atVoice) takePending() *atCall {
	v.mu.Lock()
	defer v.mu.Unlock()
	pending := v.pending
	v.pending = nil
	return pending
}

func (v *atVoice) setPending(call *atCall) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.pending = call
}

func (v *atVoice) snapshot() []*atCall {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]*atCall, 0, len(v.calls))
	for _, call := range v.calls {
		out = append(out, call)
	}
	return out
}

func (v *atVoice) forget(index int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.calls, index)
}

func (v *atVoice) update(call *atCall, state string, held bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	call.State = state
	call.Held = held
}

func atCallDirection(call modemvoice.Call) string {
	if call.Inbound {
		return "inbound"
	}
	return "outbound"
}

// atCallState 把 CLCC 状态映射成平台统一的通话状态。
func atCallState(state modemvoice.State) (string, string) {
	switch state {
	case modemvoice.Active:
		return "connected", "CallAnswered"
	case modemvoice.Held:
		return "connected", "CallAnswered"
	case modemvoice.Dialing:
		return "calling", "CallRinging"
	case modemvoice.Alerting, modemvoice.Incoming:
		return "ringing", "CallRinging"
	case modemvoice.Waiting:
		return "waiting", "CallWaiting"
	}
	return "calling", ""
}

// atFallbackProbe 是 AT 侧对"IMS 是否可用"的判定依据。
type atFallbackProbe struct {
	IMSEnabled    bool
	IMSPDN        bool
	Registered    bool // LTE 已附着
	IMSRegistered bool // 模块自身 IMS 已注册
	IMSOn         bool // +QCFG="ims" 是否成功解析
}

// atRegistration 是 AT 侧的一次注册探测结果。
type atRegistration struct {
	LTERegistered bool
	IMSRegistered bool
	IMSPDN        bool
	// Known 表示关键项都查到了：串口偶发超时不该把已注册的 IMS 打回未注册。
	Known bool
}

func (c *Controller) atFallback(deviceID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sess[strings.TrimSpace(deviceID)]
	return s != nil && s.atVoice != nil
}

func (c *Controller) atVoiceSession(deviceID string) *atVoice {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sess[strings.TrimSpace(deviceID)]
	if s == nil {
		return nil
	}
	return s.atVoice
}

// qmiAvailable 判断设备是否有可用的 QMI 核心。Host 不实现该接口时按"有 QMI"
// 处理：宁可不降级，也不要把一条本来正常的 QMI 链路切到 AT。
func (c *Controller) qmiAvailable(deviceID string) bool {
	if c == nil || c.host == nil {
		return false
	}
	if probe, ok := c.host.(interface{ HasQMICore(string) bool }); ok {
		return probe.HasQMICore(deviceID)
	}
	return true
}

// atCheckRegistration 用 AT 查一次注册状态：
//   - AT+CEREG?：LTE 附着；
//   - AT+CIREG?：模块自身 IMS 注册位（EC20 等不支持时退化成看 ims PDN）；
//   - AT+CGDCONT? + AT+CGACT?：ims APN 的 PDP 是否真的激活。
func (c *Controller) atCheckRegistration(deviceID string) atRegistration {
	var reg atRegistration
	lteKnown := false
	if response, err := execAT(c.host, deviceID, CEREGQueryCommand()); err == nil {
		if lte, parseErr := ParseCEREG(response); parseErr == nil {
			lteKnown = true
			reg.LTERegistered = lte.Registered
		}
	}
	ciregOK := false
	if response, err := execAT(c.host, deviceID, CIREGQueryCommand()); err == nil {
		if ims, parseErr := ParseCIREG(response); parseErr == nil {
			ciregOK = true
			reg.IMSRegistered = ims
		}
	}
	pdnKnown := false
	if response, err := execAT(c.host, deviceID, CGDCONTQueryCommand()); err == nil {
		contexts, parseErr := ParseCGDCONT(response)
		if imsCtx, ok := IMSContext(contexts); parseErr == nil && ok {
			if actResp, actErr := execAT(c.host, deviceID, CGACTQueryCommand()); actErr == nil {
				if active, actParseErr := ParseCGACT(actResp); actParseErr == nil {
					pdnKnown = true
					reg.IMSPDN = active[imsCtx.CID]
				}
			}
		}
	}
	if !ciregOK {
		// 模块不认 CIREG（EC20F 实测返回 ERROR）：IMS PDN 激活说明模块已经把
		// IMS 注册流程跑起来了，用它替代注册位，避免 LTE 一附着就谎报 IMS 就绪。
		reg.IMSRegistered = reg.LTERegistered && reg.IMSPDN
	}
	reg.Known = lteKnown && (ciregOK || pdnKnown)
	return reg
}

// probeATFallback 用 AT 确认模块自身能跑 VoLTE：
//   - AT+QCFG="ims"：IMS 开关为 0 时补写一次（模块重启后这条设置会丢）；
//   - AT+CGDCONT?：存在 "ims" APN 上下文；
//   - AT+CEREG?/AT+CIREG?：LTE 已注册，模块会自己完成 IMS 注册。
func (c *Controller) probeATFallback(ctx context.Context, deviceID string) (atFallbackProbe, error) {
	if c == nil || c.host == nil {
		return atFallbackProbe{}, errors.New("volte: controller is not configured")
	}
	var probe atFallbackProbe
	if ctx != nil && ctx.Err() != nil {
		return probe, ctx.Err()
	}
	response, err := execAT(c.host, deviceID, IMSQueryCommand())
	if err == nil {
		if cfg, parseErr := ParseIMSConfig(response); parseErr == nil {
			probe.IMSOn = true
			probe.IMSEnabled = cfg.IMSEnabled
		}
	}
	if !probe.IMSEnabled {
		for _, command := range IMSSetCommands(true, true) {
			if _, err := execAT(c.host, deviceID, command); err == nil {
				probe.IMSEnabled = true
				logger.Info("VoLTE 降级：已用 AT 打开模块 IMS", "device", deviceID, "cmd", command)
				break
			}
		}
	}
	reg := c.atCheckRegistration(deviceID)
	probe.Registered = reg.LTERegistered
	probe.IMSRegistered = reg.IMSRegistered
	probe.IMSPDN = reg.IMSPDN
	return probe, nil
}

// enableATFallback 在 QMI 缺失时接管注册判定，并把会话切成 at_fallback。
// 返回 nil 表示降级成立，上层不需要再报错。
func (c *Controller) enableATFallback(ctx context.Context, deviceID string, cause error) error {
	probe, err := c.probeATFallback(ctx, deviceID)
	if err != nil {
		return err
	}
	c.startATVoice(deviceID)
	c.patch(deviceID, func(st *Status) {
		st.QMIIMSUnavailable = true
		st.ATFallback = true
		// provision 阶段为了开 UAC 会置 reboot_required；AT 降级用主机 ALSA 出声，
		// 既不需要 UAC 也不需要重启模组，留着只会让首页一直提示"需重启模组"。
		st.RebootRequired = false
		st.IMSEnabled = probe.IMSEnabled
		st.VoLTEEnabled = probe.IMSEnabled
		st.IMSPDNActive = probe.IMSPDN
		st.LTERegistered = probe.Registered
		st.IMSRegistered = probe.IMSRegistered
		st.VoiceAvailable = probe.IMSRegistered
		st.VoiceIndicationsReady = probe.IMSRegistered
		if probe.IMSRegistered {
			st.Phase = PhaseRegistered
			st.LastError = ""
			return
		}
		st.Phase = PhaseRegistering
		st.LastError = fmt.Sprintf("VoLTE 降级为 AT 模式，等待模组 IMS 注册（LTE 已附着=%t，IMS PDN=%t）",
			probe.Registered, probe.IMSPDN)
	})
	if !probe.IMSRegistered {
		// 开卡/上电瞬间 ims PDN 往往还在激活，模块会自己注册 IMS；
		// 这里不判死，交给后台轮询把 phase 推到 registered。
		logger.Warn("VoLTE 已降级到 AT 模式，模块 IMS 尚未注册，转入后台轮询",
			"device", deviceID, "lte", probe.Registered, "ims_pdn", probe.IMSPDN, "cause", cause)
		return nil
	}
	logger.Warn("VoLTE 已降级到 AT 模式（模块无 QMI，用 ATD+CLCC 通话）",
		"device", deviceID, "ims", probe.IMSEnabled, "ims_pdn", probe.IMSPDN, "cause", cause)
	return nil
}

func (c *Controller) startATVoice(deviceID string) *atVoice {
	deviceID = strings.TrimSpace(deviceID)
	c.mu.Lock()
	if existing := c.ensureLocked(deviceID).atVoice; existing != nil {
		c.mu.Unlock()
		return existing
	}
	client, err := modemvoice.NewClient(atVoiceATPort{host: c.host, deviceID: deviceID})
	if err != nil {
		c.mu.Unlock()
		logger.Error("VoLTE 降级：AT 语音通道初始化失败", "device", deviceID, "err", err)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	voice := &atVoice{deviceID: deviceID, client: client, cancel: cancel, done: make(chan struct{}), calls: map[int]*atCall{}}
	c.ensureLocked(deviceID).atVoice = voice
	c.mu.Unlock()
	go c.runATVoice(ctx, voice)
	return voice
}

func (c *Controller) stopATVoice(deviceID string) {
	c.mu.Lock()
	s := c.sess[strings.TrimSpace(deviceID)]
	var voice *atVoice
	if s != nil {
		voice = s.atVoice
		s.atVoice = nil
	}
	c.mu.Unlock()
	if voice == nil {
		return
	}
	voice.cancel()
	select {
	case <-voice.done:
	case <-time.After(atHangupReconcile):
	}
}

func (c *Controller) runATVoice(ctx context.Context, voice *atVoice) {
	defer close(voice.done)
	for {
		if ctx.Err() != nil {
			return
		}
		calls, err := voice.client.Calls(ctx)
		if err == nil {
			c.applyATCalls(voice, calls)
		} else if ctx.Err() == nil {
			logger.Debug("VoLTE 降级：CLCC 轮询失败", "device", voice.deviceID, "err", err)
		}
		interval := atFallbackPollIdle
		if voice.busy() {
			interval = atFallbackPollBusy
		} else {
			// 空闲时顺带核对注册状态：模块掉网/IMS 掉注册不会有 URC 通知。
			c.refreshATRegistration(ctx, voice.deviceID)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (c *Controller) applyATCalls(voice *atVoice, calls []modemvoice.Call) {
	now := time.Now()
	seen := make(map[int]bool, len(calls))
	for _, call := range calls {
		// 只把语音通话纳入状态机：数据/传真条目会让 ATA/CHUP 打到错误的对象上。
		if call.Mode != 0 {
			continue
		}
		seen[call.Index] = true
		tracked, created := voice.bind(call.Index, call, now)
		c.atApplyCall(voice, tracked, call, created, now)
	}
	for _, tracked := range voice.snapshot() {
		if seen[tracked.Index] {
			voice.markSeen(tracked)
			continue
		}
		// EC20F 的 CLCC 偶发少列一路通话，等待中的那路尤其容易丢。单次缺失
		// 就当成挂断会重建 CallID，上层把这通来电判成新的、再判忙拒掉，
		// 界面上最后连挂断的入口都没有。
		if voice.markMissed(tracked, atCallMissTolerance) {
			continue
		}
		voice.forget(tracked.Index)
		c.atEndCall(voice, tracked, "remote_hangup")
	}
}

// atCallMissTolerance 是 CLCC 连续漏掉一通电话多少次才判定它已经结束。
// busy 轮询 800ms 一次，两次约 1.6 秒，够覆盖模组的偶发半截快照。
const atCallMissTolerance = 2

func (v *atVoice) markSeen(call *atCall) {
	v.mu.Lock()
	call.Misses = 0
	v.mu.Unlock()
}

func (v *atVoice) markMissed(call *atCall, tolerance int) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	call.Misses++
	return call.Misses < tolerance
}

func (c *Controller) atApplyCall(voice *atVoice, tracked *atCall, call modemvoice.Call, created bool, now time.Time) {
	deviceID := voice.deviceID
	state, kind := atCallState(call.State)
	held := call.State == modemvoice.Held
	voice.update(tracked, state, held)
	if peer := strings.TrimSpace(call.Number); peer != "" {
		tracked.Peer = peer
	}

	if state == "connected" {
		tracked.Connected = true
	}
	c.storeATCall(voice, tracked)

	if tracked.Direction == "inbound" && !tracked.Rang && created && call.State == modemvoice.Incoming {
		tracked.Rang = true
		c.atIncoming(voice, tracked, now)
	}

	if kind == "" {
		return
	}
	vs := c.sessionVoice(deviceID)
	if vs == nil || !vs.markEmitted(tracked.ID, rankKey(state)) {
		return
	}
	logger.Info("VoLTE(AT) 通话状态", "device", deviceID, "clcc_index", call.Index,
		"state", state, "event", kind, "direction", tracked.Direction, "peer", tracked.Peer)
	c.emitEvent(deviceID, voicehost.CallEvent{
		Type: kind, DeviceID: deviceID, CallID: tracked.ID, Caller: tracked.Peer, Callee: tracked.Peer,
		Direction: tracked.Direction, State: state, Time: now,
		RecordingError: audioError(c.Status(deviceID)),
	})
	if kind == "CallAnswered" && c.audio != nil {
		if err := c.audio.Start(deviceID, tracked.ID); err != nil {
			c.setError(deviceID, err)
		}
	}
}

func (c *Controller) atIncoming(voice *atVoice, tracked *atCall, now time.Time) {
	deviceID := voice.deviceID
	sdp := ""
	if media, err := startCallMedia("", c.callPCM(deviceID), false); err == nil {
		sdp = media.sdp
		c.media.put(tracked.ID, media)
	} else {
		logger.Warn("VoLTE(AT) 来电媒体端点未建立", "device", deviceID, "err", err)
	}
	c.mu.Lock()
	if s := c.sess[deviceID]; s != nil && s.voice != nil {
		s.voice.markIncoming(tracked.ID)
	}
	c.mu.Unlock()
	c.emitIncoming(deviceID, voicehost.IncomingCall{
		DeviceID: deviceID, CallID: tracked.ID, Caller: tracked.Peer, State: "ringing",
		ReceivedAt: now, OfferSDP: sdp,
	})
}

func (c *Controller) atEndCall(voice *atVoice, tracked *atCall, reason string) {
	deviceID := voice.deviceID
	voiced := c.sessionVoice(deviceID)
	if voiced != nil && !voiced.markEmitted(tracked.ID, rankKey("completed")) {
		return
	}
	c.releaseCallAudio(tracked.ID)
	c.noteVoiceActivity(deviceID)
	if voiced != nil {
		voiced.forget(tracked.ID)
	}
	if strings.TrimSpace(reason) == "" {
		reason = "remote_hangup"
	}
	// 没接起来就被挂断的来电按 CallCanceled 上报，和 QMI 路径保持一致。
	eventType := "CallEnded"
	if tracked.Direction == "inbound" && !tracked.Connected {
		eventType = "CallCanceled"
	}
	c.emitEvent(deviceID, voicehost.CallEvent{
		Type: eventType, DeviceID: deviceID, CallID: tracked.ID,
		Caller: tracked.Peer, Callee: tracked.Peer, Direction: tracked.Direction,
		State: "completed", Time: time.Now(), Reason: reason,
		RecordingError: audioError(c.Status(deviceID)),
	})
	if tracked.Connected {
		c.emitCallFinalized(deviceID, tracked.ID, tracked.Peer, tracked.Direction, "", reason, time.Now())
	}
}

// storeATCall 把降级通话镜像进 voiceSession，ActiveCall/lookup 才能看到它。
func (c *Controller) storeATCall(voice *atVoice, tracked *atCall) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sess[voice.deviceID]
	if s == nil || s.voice == nil {
		return
	}
	index := tracked.Index
	if index < 0 {
		index = 0
	}
	s.voice.put(nativeCall{
		ID: tracked.ID, QMI: uint8(index), Direction: tracked.Direction, Peer: tracked.Peer,
		State: tracked.State, Start: tracked.Start, Held: tracked.Held,
		ClientSDP: tracked.ClientSDP,
	})
}

func (c *Controller) atBeginCall(ctx context.Context, deviceID, callee, sdp string) (voicehost.CallSnapshot, error) {
	voice := c.atVoiceSession(deviceID)
	if voice == nil {
		return voicehost.CallSnapshot{}, errors.New("volte: AT 降级会话未启动")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.armVoicePCM(deviceID)
	if err := voice.client.Dial(ctx, callee); err != nil {
		return voicehost.CallSnapshot{}, err
	}
	now := time.Now()
	// ATD 被接受不代表呼叫已建立：先用一次 CLCC 认领 index，认领不到就挂到
	// pending 上，等轮询把它绑定到同一个 ID。
	tracked := &atCall{
		ID: newPersistentCallID(deviceID, 0), Peer: callee, Direction: "outbound",
		State: "calling", Start: now,
	}
	bound := false
	if calls, err := voice.client.Calls(ctx); err == nil {
		for _, call := range calls {
			if call.Mode != 0 || call.Inbound {
				continue
			}
			tracked.Index = call.Index
			tracked.ID = newPersistentCallID(deviceID, uint8(call.Index))
			voice.bindIndex(call.Index, tracked)
			bound = true
			break
		}
	}
	if !bound {
		tracked.Index = -1
		voice.setPending(tracked)
	}
	c.storeATCall(voice, tracked)
	if vs := c.sessionVoice(deviceID); vs != nil {
		vs.markEmitted(tracked.ID, rankKey("calling"))
	}
	media, mediaErr := startCallMedia(sdp, c.callPCM(deviceID), sdpHasRecvOnly(sdp))
	callSDP := ""
	if mediaErr != nil {
		logger.Warn("VoLTE(AT) 媒体端点未建立", "device", deviceID, "err", mediaErr)
	} else {
		callSDP = media.sdp
		c.media.put(tracked.ID, media)
		// 端点就绪后补一次镜像，让 ActiveCall 能带上 ClientSDP。
		tracked.ClientSDP = callSDP
		c.storeATCall(voice, tracked)
	}
	c.emitEvent(deviceID, voicehost.CallEvent{
		Type: "CallRinging", DeviceID: deviceID, CallID: tracked.ID, Callee: callee,
		Direction: "outbound", State: "calling", Time: now,
		RecordingError: audioError(c.Status(deviceID)),
	})
	return voicehost.CallSnapshot{
		CallID: tracked.ID, DeviceID: deviceID, State: "calling", Direction: "outbound",
		Peer: callee, StartTime: now, ClientSDP: callSDP,
	}, nil
}

func (v *atVoice) bindIndex(index int, call *atCall) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls[index] = call
}

func (c *Controller) atAnswer(ctx context.Context, deviceID, callID string) error {
	voice := c.atVoiceSession(deviceID)
	if voice == nil {
		return errors.New("volte: AT 降级会话未启动")
	}
	tracked := voice.byID(callID)
	if tracked == nil {
		return fmt.Errorf("volte: 通话 %s 不存在", callID)
	}
	calls, err := voice.client.Calls(ctx)
	if err != nil {
		return err
	}
	for _, call := range calls {
		if call.Index != tracked.Index || call.Mode != 0 {
			continue
		}
		c.armVoicePCM(deviceID)
		if err := voice.client.Answer(ctx, call); err != nil {
			return err
		}
		return nil
	}
	return errors.New("volte: 来电已结束")
}

func (c *Controller) atHangup(ctx context.Context, deviceID, callID string) error {
	voice := c.atVoiceSession(deviceID)
	if voice == nil {
		return errors.New("volte: AT 降级会话未启动")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tracked := voice.byID(callID)
	if tracked == nil {
		// pending 里还没被 CLCC 认领的呼出：直接挂断，别让它变成幽灵通话。
		if pending := voice.takePending(); pending != nil && pending.ID == callID {
			_, err := c.host.ExecuteAT(deviceID, "AT+CHUP", atDtmfTimeout)
			voice.forget(-1)
			c.atEndCall(voice, pending, "local_hangup")
			return err
		}
		return nil
	}
	calls, err := voice.client.Calls(ctx)
	if err != nil {
		return err
	}
	for _, call := range calls {
		if call.Index != tracked.Index || call.Mode != 0 {
			continue
		}
		if err := voice.client.Hangup(ctx, call); err != nil {
			// CHUP 只影响语音通话，失败时退回全局 ATH 再试一次。
			logger.Warn("VoLTE(AT) 挂断失败，退回 ATH", "device", deviceID, "err", err)
			if _, retryErr := c.host.ExecuteAT(deviceID, "ATH", atDtmfTimeout); retryErr != nil {
				return fmt.Errorf("volte: AT 挂断失败: %w", retryErr)
			}
		}
		break
	}
	// ATH/CHUP 返回 OK 并不代表网络侧真的释放（EC20 实测会残留），必须对账 CLCC；
	// 残留时补一次 CHUP——它只挂语音，不会误伤同时存在的数据呼叫。
	if !c.atWaitCallGone(ctx, voice, tracked.Index, atHangupReconcile) {
		logger.Warn("VoLTE(AT) 挂断后 CLCC 仍残留，补发 CHUP", "device", deviceID, "clcc_index", tracked.Index)
		_, _ = c.host.ExecuteAT(deviceID, "AT+CHUP", atDtmfTimeout)
		if !c.atWaitCallGone(ctx, voice, tracked.Index, atHangupReconcile) {
			logger.Warn("VoLTE(AT) 通话仍未释放，请重启模组", "device", deviceID, "clcc_index", tracked.Index)
			return errors.New("volte: 模块未释放通话")
		}
	}
	voice.forget(tracked.Index)
	c.atEndCall(voice, tracked, "local_hangup")
	return nil
}

// atWaitCallGone 轮询 CLCC 直到该 index 的语音通话消失。
func (c *Controller) atWaitCallGone(ctx context.Context, voice *atVoice, index int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		remaining, err := voice.client.Calls(ctx)
		if err == nil && !atCallPresent(remaining, index) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func atCallPresent(calls []modemvoice.Call, index int) bool {
	for _, call := range calls {
		if call.Index == index && call.Mode == 0 {
			return true
		}
	}
	return false
}

func (c *Controller) atSendDTMF(ctx context.Context, deviceID, callID, digit string) error {
	voice := c.atVoiceSession(deviceID)
	if voice == nil {
		return errors.New("volte: AT 降级会话未启动")
	}
	tracked := voice.byID(callID)
	if tracked == nil {
		return fmt.Errorf("volte: 通话 %s 不存在", callID)
	}
	if tracked.State != "connected" {
		return errors.New("volte: 只有接通后的通话能发 DTMF")
	}
	if len(digit) != 1 || !strings.Contains("0123456789*#", digit) {
		return errors.New("volte: DTMF 只支持 0-9、*、#")
	}
	command := fmt.Sprintf("AT+VTS=\"%s\",%d", digit, atDtmfDurationTenths)
	_, err := c.host.ExecuteAT(deviceID, command, atDtmfTimeout)
	return err
}

func (c *Controller) atSetHold(ctx context.Context, deviceID, callID string, hold bool) error {
	voice := c.atVoiceSession(deviceID)
	if voice == nil {
		return errors.New("volte: AT 降级会话未启动")
	}
	tracked := voice.byID(callID)
	if tracked == nil {
		return fmt.Errorf("volte: 通话 %s 不存在", callID)
	}
	if tracked.Held == hold {
		return nil
	}
	// CHLD=2 在单通话场景就是"保持/恢复"的开关（3GPP TS 27.007 §7.13）。
	if _, err := c.host.ExecuteAT(deviceID, "AT+CHLD=2", atDtmfTimeout); err != nil {
		action := "保持"
		if !hold {
			action = "恢复"
		}
		return fmt.Errorf("volte: %s通话失败: %w", action, err)
	}
	voice.update(tracked, tracked.State, hold)
	c.storeATCall(voice, tracked)
	c.emitEvent(deviceID, voicehost.CallEvent{
		Type: "CallMediaUpdated", DeviceID: deviceID, CallID: tracked.ID,
		Caller: tracked.Peer, Callee: tracked.Peer, Direction: tracked.Direction,
		State: tracked.State, Time: time.Now(), Held: hold,
		RecordingError: audioError(c.Status(deviceID)),
	})
	return nil
}

// atPCMVRouting 在 AT 降级下确认语音路由指向 USB 声卡。失败只记日志：
// QPCMV 的失败不代表声卡不可用，别因此把通话降成无声。
func (c *Controller) atApplyPCMV(deviceID string) {
	if c == nil || c.host == nil {
		return
	}
	if _, err := c.host.ExecuteAT(deviceID, "AT+QPCMV=1,2", atPCMVTimeout); err != nil {
		logger.Warn("VoLTE(AT) 下发 QPCMV 失败", "device", deviceID, "err", err)
	}
}
