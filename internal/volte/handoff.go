package volte

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
	"github.com/yibaiba/hideck/internal/modemvoice"
)

// DisableForHandoff confirms physical calls have ended before another backend
// takes ownership. Control failure is not evidence that the modem hung up.
func (c *Controller) DisableForHandoff(ctx context.Context, deviceID string) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("volte: handoff context is required")
	}
	deviceID = strings.TrimSpace(deviceID)
	return c.withDevice(deviceID, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		s := c.sess[deviceID]
		c.mu.Unlock()
		if s == nil {
			return nil
		}
		if c.atFallback(deviceID) {
			// AT 降级没有 QMI 核：VOICEGetAllCallInfo/ReleaseIMSClients 在这类
			// 设备上必然失败，交接不能因此判死。通话事实改由 CLCC 确认，交接
			// 完成后整个会话（含 CLCC 轮询）一起交出去。
			if err := c.endATCallsForHandoff(ctx, deviceID); err != nil {
				return err
			}
			if active := c.ActiveCall(deviceID); active != nil {
				if m := c.media.take(active.CallID); m != nil {
					_ = m.Close()
				}
			}
			c.dropSession(deviceID)
			return nil
		}
		if err := c.endCallsForHandoff(ctx, deviceID); err != nil {
			return err
		}
		if err := c.host.ReleaseIMSClients(deviceID); err != nil {
			return fmt.Errorf("volte: release IMS clients for handoff: %w", err)
		}
		// 走到这里说明当前不是 AT 降级，但历史轮询可能还留着，一并停掉。
		c.stopATVoice(deviceID)
		c.mu.Lock()
		s.gen++
		delete(c.sess, deviceID)
		c.mu.Unlock()
		return nil
	})
}

// endATCallsForHandoff 用 CLCC 确认 AT 降级链路没有还在进行的通话。
// 语义与 QMI 版本一致：挂断成功后必须复核，仍有通话就拒绝交接，避免把
// 正在通话的模组交给另一个后端（模组直拨会重新下发 ATD）。
func (c *Controller) endATCallsForHandoff(ctx context.Context, deviceID string) error {
	voice := c.atVoiceSession(deviceID)
	if voice == nil {
		return nil
	}
	calls, err := voice.client.Calls(ctx)
	if err != nil {
		return fmt.Errorf("volte: query AT calls for handoff: %w", err)
	}
	if calls = atVoiceCalls(calls); len(calls) > 0 {
		// 交接要把语音通话全部结束：单路走 modemvoice 的定向挂断（它不会误挂
		// 数据呼叫），多路或定向失败时退回 AT+CHUP（一次挂断全部语音通话）。
		if len(calls) == 1 {
			if hangupErr := voice.client.Hangup(ctx, calls[0]); hangupErr == nil {
				calls = nil
			}
		}
		if calls != nil {
			if _, chupErr := c.host.ExecuteAT(deviceID, "AT+CHUP", atHangupReconcile); chupErr != nil {
				return fmt.Errorf("volte: hang up AT calls for handoff: %w", chupErr)
			}
		}
	}
	remaining, err := voice.client.Calls(ctx)
	if err != nil {
		return fmt.Errorf("volte: re-query AT calls for handoff: %w", err)
	}
	if remaining = atVoiceCalls(remaining); len(remaining) > 0 {
		return fmt.Errorf("volte: AT call %d is still active; retry switching after it ends", remaining[0].Index)
	}
	return nil
}

// atVoiceCalls 只保留语音通话：CLCC 里的 mode 0 是语音，数据/传真呼叫在
// 交接时不应该挡住切换，也不应该被 CHUP 之外的动作牵连。
func atVoiceCalls(calls []modemvoice.Call) []modemvoice.Call {
	voice := make([]modemvoice.Call, 0, len(calls))
	for _, call := range calls {
		if call.Mode == 0 {
			voice = append(voice, call)
		}
	}
	return voice
}

func (c *Controller) endCallsForHandoff(ctx context.Context, deviceID string) error {
	info, err := c.callsForHandoff(ctx, deviceID)
	if err != nil {
		return err
	}
	for _, call := range info.Calls {
		if call.State == qmiCallIdle || call.State == qmiCallEnd {
			continue
		}
		err := c.host.VOICEHangup(ctx, deviceID, call.ID)
		if err != nil && !qmi.VoiceCallAlreadyGone(err) {
			return fmt.Errorf("volte: hang up call %d for handoff: %w", call.ID, err)
		}
	}
	// An accepted hangup alone is insufficient: pending disconnects or new
	// incoming calls leave ownership here until an explicit retry succeeds.
	info, err = c.callsForHandoff(ctx, deviceID)
	if err != nil {
		return err
	}
	for _, call := range info.Calls {
		if call.State != qmiCallIdle && call.State != qmiCallEnd {
			return fmt.Errorf("volte: call %d is still active; retry switching after it ends", call.ID)
		}
	}
	if vs := c.sessionVoice(deviceID); vs != nil {
		c.handleVoiceInfo(deviceID, vs, info)
	}
	return nil
}

func (c *Controller) callsForHandoff(ctx context.Context, deviceID string) (*qmi.VoiceAllCallInfo, error) {
	if c.host == nil {
		return nil, errors.New("volte: controller is not configured")
	}
	info, err := c.host.VOICEGetAllCallInfo(ctx, deviceID)
	if err != nil {
		return nil, fmt.Errorf("volte: query calls for handoff: %w", err)
	}
	if info == nil {
		return nil, errors.New("volte: empty call query response during handoff")
	}
	return info, nil
}
