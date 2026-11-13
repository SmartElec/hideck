package phone

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/yibaiba/hideck/pkg/logger"
)

var calleePattern = regexp.MustCompile(`^\+?[0-9]{1,32}$`)
var dtmfPattern = regexp.MustCompile(`^[0-9*#]$`)
var errActiveCallNotFound = errors.New("phone: active call not found")

// ErrHoldUnavailable is returned when originating hold/resume is refused as not aligned.
var ErrHoldUnavailable = errors.New("保持未对齐，暂不可用")

func (s *Service) StartCall(request StartCallRequest) (CallView, error) {
	request.DeviceID, request.Callee = strings.TrimSpace(request.DeviceID), strings.TrimSpace(request.Callee)
	if request.DeviceID == "" || !calleePattern.MatchString(request.Callee) {
		return CallView{}, errors.New("phone: device and a valid callee are required")
	}
	media, reservation, err := s.reserveControlledMedia(request.Owner, request.MediaID, request.Lease)
	if err != nil {
		return CallView{}, err
	}
	defer s.releaseMediaReservation(reservation)
	request.MediaID = reservation.mediaID
	if err := s.reserveDevice(request.DeviceID); err != nil {
		return CallView{}, err
	}
	startedAt := time.Now()
	snapshot, err := s.gateway.BeginCall(s.ctx, voicehost.BeginCallRequest{
		DeviceID: request.DeviceID, Callee: request.Callee, SDP: media.PlainSDP(),
		CaptureBasePath: s.captureBase(request.DeviceID, startedAt),
	})
	// A backend may retain a physical call after a failed dial/cleanup. Keep
	// that call controllable while still returning the original failure.
	if err != nil && snapshot.CallID == "" {
		s.releaseDeviceReservation(request.DeviceID)
		return CallView{}, err
	}
	call := s.newOutboundCall(request, snapshot, startedAt)
	s.mu.Lock()
	pendingMediaDrop, bindErr := s.bindMediaLocked(call.view.CallID, reservation)
	if bindErr == nil {
		s.calls[call.view.CallID] = call
		s.deviceCalls[call.view.DeviceID] = call.view.CallID
	}
	pendingEvents := s.pendingEvents[call.view.CallID]
	delete(s.pendingEvents, call.view.CallID)
	s.mu.Unlock()
	if bindErr != nil {
		s.releaseDeviceReservation(request.DeviceID)
		hangupErr := s.gateway.HangupCall(s.ctx, call.view.DeviceID, call.view.CallID)
		return CallView{}, errors.Join(bindErr, hangupErr)
	}
	s.resumePendingMediaDrop(call.mediaID, pendingMediaDrop)
	// 出站呼叫一拨出去就把浏览器媒体接到模组侧：回铃音、运营商提示音
	// （"正在通话中"/"不在服务区"）都是响铃阶段的早期媒体，等到接通才接媒体
	// 会让浏览器整段响铃完全静音，用户既听不到回铃也听不到忙音提示。
	s.attachEarlyOutboundMedia(call.view.CallID, call.mediaID, snapshot.ClientSDP)
	s.persist(call.record)
	s.publish("call_started", call)
	for _, event := range pendingEvents {
		s.dispatchCallEvent(event)
	}
	return s.callView(call.view.CallID, request.Lease), err
}

// attachEarlyOutboundMedia 在响铃阶段把浏览器媒体接到模组侧端点。
//
// 与接通后的 attachOutboundMedia 不同：这里失败只告警、绝不挂断。部分后端要等
// 接通才给出协商结果，此时没有端点属于正常；真到接通还拿不到，由
// attachOutboundMedia 的严格检查去终结这通电话。
func (s *Service) attachEarlyOutboundMedia(callID, mediaID, clientSDP string) {
	clientSDP = strings.TrimSpace(clientSDP)
	if clientSDP == "" {
		return
	}
	media := s.media.Get(mediaID)
	if media == nil {
		return
	}
	if err := media.Attach(clientSDP); err != nil {
		logger.Warn("出站通话响铃阶段媒体桥接失败，接通后再试",
			"call_id", callID, "media_id", mediaID, "err", err)
	}
}

func (s *Service) newOutboundCall(
	request StartCallRequest,
	snapshot voicehost.CallSnapshot,
	startedAt time.Time,
) *activeCall {
	view := CallView{
		CallID: snapshot.CallID, DeviceID: request.DeviceID, Direction: "outbound",
		Peer: request.Callee, Status: StatusCalling, MediaID: request.MediaID, StartedAt: startedAt,
	}
	record := CallRecord{
		CallID: snapshot.CallID, DeviceID: request.DeviceID, ICCID: s.iccid(request.DeviceID),
		Direction: "outbound", Peer: request.Callee, Status: StatusCalling, StartedAt: startedAt,
	}
	if endpoint, err := parseRTPEndpoint(snapshot.ClientSDP); err == nil && endpoint.Codec != "" {
		view.Codec, record.Codec = endpoint.Codec, endpoint.Codec
	}
	return &activeCall{
		view: view, record: record, owner: request.Owner, lease: request.Lease,
		mediaID: request.MediaID, recordingBase: s.captureBase(request.DeviceID, startedAt),
		terminalDone: make(chan struct{}), finalizedDone: make(chan struct{}),
	}
}

func (s *Service) Answer(ctx context.Context, request ControlRequest) (CallView, error) {
	call, media, reservation, err := s.claimIncoming(request)
	if err != nil {
		return CallView{}, err
	}
	defer s.releaseIncomingClaim(call)
	defer s.releaseMediaReservation(reservation)
	s.mu.RLock()
	incomingSDP, deviceID, callID := call.incomingSDP, call.view.DeviceID, call.view.CallID
	s.mu.RUnlock()
	if err := media.Attach(incomingSDP); err != nil {
		rejectErr := s.gateway.RejectIncomingCall(voicehost.RejectRequest{
			DeviceID: deviceID, CallID: callID, StatusCode: 488,
		})
		return CallView{}, errors.Join(err, rejectErr)
	}
	answer, err := s.gateway.AnswerIncomingCall(ctx, voicehost.AnswerRequest{
		DeviceID: deviceID, CallID: callID, SDP: media.PlainSDP(),
	})
	if err != nil {
		return CallView{}, err
	}
	if answer.OfferSDP != "" && answer.OfferSDP != incomingSDP {
		if err := media.Attach(answer.OfferSDP); err != nil {
			return CallView{}, err
		}
	}
	if err := s.assignControl(call.view.CallID, reservation); err != nil {
		hangupErr := s.gateway.HangupCall(ctx, deviceID, callID)
		return CallView{}, errors.Join(err, hangupErr)
	}
	s.startMixedRecording(call, media)
	return s.callView(callID, request.Lease), nil
}

// Reject 拒接一路尚未接管的来电，全程不碰媒体会话：拒接不需要音频。
// 呼叫等待时那路来电与当前通话共用同一个 WebRTC 会话，旧实现按“接听”的
// 流程去预留媒体，只会拿到 media session already has an active call
// operation，界面上表现为这通来电根本挂不掉。
func (s *Service) Reject(request ControlRequest) error {
	callID := strings.TrimSpace(request.CallID)
	s.mu.Lock()
	call := s.calls[callID]
	if call == nil || call.terminal || call.view.Direction != "inbound" || call.controlPending {
		s.mu.Unlock()
		return errors.New("phone: pending incoming call not found")
	}
	// 已经被别的浏览器接管的来电不能从这里抢走。
	if call.lease != "" && (!secureEqual(call.lease, request.Lease) || call.owner != request.Owner) {
		s.mu.Unlock()
		return errors.New("phone: call is controlled by another browser")
	}
	call.userRejected = true
	if call.autoAnswerTimer != nil {
		call.autoAnswerTimer.Stop()
		call.autoAnswerTimer = nil
	}
	deviceID := call.view.DeviceID
	s.mu.Unlock()
	if err := s.gateway.RejectIncomingCall(voicehost.RejectRequest{
		DeviceID: deviceID, CallID: callID, StatusCode: 486,
	}); err != nil {
		return err
	}
	s.finishCall(voicehost.CallEvent{
		Type: "CallCanceled", DeviceID: deviceID, CallID: callID,
		Reason: "local_reject", Time: time.Now(),
	})
	return nil
}

func (s *Service) Hangup(ctx context.Context, owner, callID, lease string) error {
	call, err := s.hangupTarget(owner, callID, lease)
	if err != nil {
		if errors.Is(err, errActiveCallNotFound) {
			return nil
		}
		return err
	}
	s.mu.RLock()
	deviceID, resolvedCallID := call.view.DeviceID, call.view.CallID
	s.mu.RUnlock()
	if err := s.gateway.HangupCall(ctx, deviceID, resolvedCallID); err != nil {
		return err
	}
	// Native lookup can succeed the HTTP hangup without emitting CallEnded.
	s.finishCall(voicehost.CallEvent{
		Type: "CallEnded", DeviceID: deviceID, CallID: resolvedCallID,
		Reason: "local_hangup", Time: time.Now(),
	})
	return nil
}

func (s *Service) DTMF(owner, callID, lease, digit string) error {
	if !dtmfPattern.MatchString(digit) {
		return errors.New("phone: DTMF must be one of 0-9, * or #")
	}
	call, err := s.controlledCall(owner, callID, lease)
	if err != nil {
		return err
	}
	s.mu.RLock()
	status, deviceID, resolvedCallID := call.view.Status, call.view.DeviceID, call.view.CallID
	s.mu.RUnlock()
	if status != StatusConnected {
		return errors.New("phone: DTMF requires a connected call")
	}
	return s.gateway.SendCallDTMF(deviceID, resolvedCallID, digit)
}

func (s *Service) Hold(ctx context.Context, owner, callID, lease string) error {
	return s.setCallHold(ctx, owner, callID, lease, true)
}

func (s *Service) Resume(ctx context.Context, owner, callID, lease string) error {
	return s.setCallHold(ctx, owner, callID, lease, false)
}

func (s *Service) Switch(owner, callID, lease string) error {
	call, err := s.controlledCall(owner, callID, lease)
	if err != nil {
		return err
	}
	s.mu.RLock()
	deviceID, resolvedCallID := call.view.DeviceID, call.view.CallID
	s.mu.RUnlock()
	return s.gateway.SwitchCall(deviceID, resolvedCallID)
}

func (s *Service) setCallHold(ctx context.Context, owner, callID, lease string, hold bool) error {
	call, err := s.controlledCall(owner, callID, lease)
	if err != nil {
		return err
	}
	s.mu.RLock()
	status, deviceID, resolvedCallID := call.view.Status, call.view.DeviceID, call.view.CallID
	s.mu.RUnlock()
	if status != StatusConnected {
		return errors.New("phone: hold requires a connected call")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if hold {
		err = s.gateway.HoldCall(ctx, deviceID, resolvedCallID)
	} else {
		err = s.gateway.ResumeCall(ctx, deviceID, resolvedCallID)
	}
	if err != nil {
		return mapHoldError(err)
	}
	s.mu.Lock()
	if current := s.calls[resolvedCallID]; current != nil && !current.terminal {
		current.view.Held = hold
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) claimIncoming(request ControlRequest) (*activeCall, *MediaSession, *mediaReservation, error) {
	media, reservation, err := s.reserveControlledMedia(request.Owner, request.MediaID, request.Lease)
	if err != nil {
		return nil, nil, nil, err
	}
	s.mu.Lock()
	call := s.calls[request.CallID]
	if call == nil || call.terminal || call.view.Direction != "inbound" || call.controlPending {
		s.mu.Unlock()
		s.releaseMediaReservation(reservation)
		return nil, nil, nil, errors.New("phone: pending incoming call not found")
	}
	if call.lease != "" && (!secureEqual(call.lease, request.Lease) || call.owner != request.Owner) {
		s.mu.Unlock()
		s.releaseMediaReservation(reservation)
		return nil, nil, nil, errors.New("phone: call is controlled by another browser")
	}
	call.controlPending = true
	if call.autoAnswerTimer != nil {
		call.autoAnswerTimer.Stop()
		call.autoAnswerTimer = nil
	}
	s.mu.Unlock()
	return call, media, reservation, nil
}

func (s *Service) releaseIncomingClaim(call *activeCall) {
	if call == nil {
		return
	}
	s.mu.Lock()
	if current := s.calls[call.view.CallID]; current == call {
		call.controlPending = false
	}
	s.mu.Unlock()
}

func (s *Service) controlledCall(owner, callID, lease string) (*activeCall, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	call := s.calls[strings.TrimSpace(callID)]
	if call == nil || call.terminal {
		return nil, errActiveCallNotFound
	}
	if call.owner != owner || !secureEqual(call.lease, lease) {
		return nil, errors.New("phone: control lease does not own this call")
	}
	return call, nil
}

// hangupTarget 找出这条挂断请求能操作的通话。已接管的通话按 owner/lease
// 校验；尚未接管的来电（呼叫等待里的第二路）允许任何浏览器挂断——它没有
// 媒体会话也就没有租约，否则挂掉第一通之后这路来电就再也没有入口释放了。
func (s *Service) hangupTarget(owner, callID, lease string) (*activeCall, error) {
	call, err := s.controlledCall(owner, callID, lease)
	if err == nil {
		return call, nil
	}
	if errors.Is(err, errActiveCallNotFound) {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	call = s.calls[strings.TrimSpace(callID)]
	if call == nil || call.terminal {
		return nil, errActiveCallNotFound
	}
	if call.lease == "" && call.view.Direction == "inbound" &&
		(call.view.Status == StatusRinging || call.view.Status == StatusWaiting) {
		return call, nil
	}
	return nil, err
}

func (s *Service) reserveDevice(deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.deviceCalls[deviceID]; busy {
		return fmt.Errorf("phone: device %s already has an active call", deviceID)
	}
	s.deviceCalls[deviceID] = ""
	return nil
}

func (s *Service) releaseDeviceReservation(deviceID string) {
	s.mu.Lock()
	if s.deviceCalls[deviceID] == "" {
		delete(s.deviceCalls, deviceID)
	}
	s.mu.Unlock()
}

func (s *Service) assignControl(callID string, reservation *mediaReservation) error {
	s.mu.Lock()
	call := s.calls[callID]
	if call == nil || call.terminal {
		s.mu.Unlock()
		return errActiveCallNotFound
	}
	pendingMediaDrop, err := s.bindMediaLocked(callID, reservation)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	call.owner, call.lease, call.mediaID = reservation.media.Owner, reservation.media.Lease, reservation.mediaID
	call.view.MediaID = reservation.mediaID
	s.mu.Unlock()
	s.resumePendingMediaDrop(reservation.mediaID, pendingMediaDrop)
	return nil
}

func mapHoldError(err error) error {
	if errors.Is(err, voicehost.ErrHoldNotAligned) {
		return ErrHoldUnavailable
	}
	return err
}

func (s *Service) iccid(deviceID string) string {
	if s.resolveICCID == nil {
		return ""
	}
	return strings.TrimSpace(s.resolveICCID(deviceID))
}
