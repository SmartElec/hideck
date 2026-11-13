package modemvoice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	queryTimeout       = 3 * time.Second
	callCommandTimeout = 60 * time.Second
	// clccRetryDelay 只用于“列出通话但收尾报错”的半截响应，别把它当轮询间隔。
	clccRetryDelay = 250 * time.Millisecond
)

// Executor is implemented by modem.Manager and shares its SMS/AT queue.
type Executor interface {
	ExecuteATContext(context.Context, string, time.Duration) (string, error)
}

type Client struct {
	executor Executor
	gate     chan struct{}
}

func NewClient(executor Executor) (*Client, error) {
	if executor == nil {
		return nil, errors.New("modem voice: AT executor is required")
	}
	return &Client{executor: executor, gate: make(chan struct{}, 1)}, nil
}

func (c *Client) lock(ctx context.Context) error {
	if ctx == nil {
		return errors.New("modem voice: context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.gate <- struct{}{}:
		return nil
	}
}

// Calls returns only successful, fully parsed snapshots. Poll failures retain
// the previous call state in the session rather than manufacturing an empty list.
func (c *Client) Calls(ctx context.Context) ([]Call, error) {
	if err := c.lock(ctx); err != nil {
		return nil, err
	}
	defer func() { <-c.gate }()
	return c.calls(ctx)
}

func (c *Client) calls(ctx context.Context) ([]Call, error) {
	response, err := c.executor.ExecuteATContext(ctx, "AT+CLCC", queryTimeout)
	if err != nil {
		// EC20F 在通话建立/释放的瞬间偶尔先吐一段 +CLCC 列表再跟
		// +CME ERROR: 100（模组内部状态抖动，不是真的没有通话）。
		// 这种半截响应不能当空快照用，也不该直接报到界面上：稍等重试一次。
		if !partialCLCC(err) {
			return nil, fmt.Errorf("modem voice: CLCC: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("modem voice: CLCC: %w", err)
		case <-time.After(clccRetryDelay):
		}
		response, err = c.executor.ExecuteATContext(ctx, "AT+CLCC", queryTimeout)
		if err != nil {
			return nil, fmt.Errorf("modem voice: CLCC: %w", err)
		}
	}
	return ParseCLCC(response)
}

// partialCLCC 判断错误里是否夹带了 +CLCC 行：模组已经列出通话，只是收尾
// 回了错误码。真正的超时/端口错误不会有这种内容。
func partialCLCC(err error) bool {
	return err != nil && strings.Contains(err.Error(), "+CLCC:")
}

func dialCommand(number string) (string, error) {
	digits := number
	if strings.HasPrefix(digits, "+") {
		digits = digits[1:]
	}
	if digits == "" {
		return "", errors.New("modem voice: number is required")
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return "", errors.New("modem voice: invalid dial number")
		}
	}
	return "ATD" + number + ";", nil
}

// Dial never retries: a timed-out ATD can still create a network call.
// A nil result means command acceptance, not remote answer or working audio.
func (c *Client) Dial(ctx context.Context, number string) error {
	command, err := dialCommand(number)
	if err != nil {
		return err
	}
	if err := c.lock(ctx); err != nil {
		return err
	}
	defer func() { <-c.gate }()
	calls, err := c.calls(ctx)
	if err != nil {
		return err
	}
	if len(voiceControlCalls(calls)) != 0 {
		return errors.New("modem voice: device already has a call")
	}
	_, err = c.executor.ExecuteATContext(ctx, command, callCommandTimeout)
	return err
}

// CHUP affects voice calls only (EC25/EC21 manual section 7.5), unlike ATH.
// 单路通话仍然用它；有呼叫等待等多路通话时必须定向，见 controlCommand。
func (c *Client) Answer(ctx context.Context, expected Call) error {
	return c.control(ctx, expected, true)
}

func (c *Client) Hangup(ctx context.Context, expected Call) error {
	return c.control(ctx, expected, false)
}

func (c *Client) control(ctx context.Context, expected Call, answer bool) error {
	if err := c.lock(ctx); err != nil {
		return err
	}
	defer func() { <-c.gate }()
	calls, err := c.calls(ctx)
	if err != nil {
		return err
	}
	voice := voiceControlCalls(calls)
	target, ok := pickControlCall(voice, expected)
	if !ok {
		return errors.New("modem voice: call changed or requires targeted control")
	}
	command, err := controlCommand(target, voice, answer)
	if err != nil {
		return err
	}
	_, err = c.executor.ExecuteATContext(ctx, command, callCommandTimeout)
	return err
}

// pickControlCall 在多路通话里定位要操作的那一路。CLCC 索引由模组分配，
// 索引本身才是稳定的句柄，所以先按索引找，再用 sameCall 确认方向/号码/
// 状态没有被后续通话复用。
func pickControlCall(calls []Call, expected Call) (Call, bool) {
	for _, call := range calls {
		if sameCall(expected, call) {
			return call, true
		}
	}
	return Call{}, false
}

// controlCommand 生成定向控制命令。只有一路语音通话时沿用 AT+CHUP / ATA，
// 这条路已经在线验证过；出现呼叫等待等多路通话时不能再发整段命令：
// AT+CHLD=1<idx> 只释放指定索引，AT+CHLD=2 保留当前通话并接听等待的那一路。
//
// 待定的数据/传真通话（mode != 0）会让“接听”变成歧义：ATA 可能接起数据呼叫，
// 所以有别的非语音通话时继续拒绝接听，只允许定向挂断。
func controlCommand(target Call, calls []Call, answer bool) (string, error) {
	if target.Mode != 0 || target.Multiparty {
		return "", errors.New("modem voice: call cannot be controlled as voice")
	}
	if answer {
		if !target.Inbound || (target.State != Incoming && target.State != Waiting) {
			return "", errors.New("modem voice: call is not an incoming call")
		}
		for _, call := range calls {
			if call.Index != target.Index && call.Mode != 0 {
				return "", errors.New("modem voice: pending data call blocks answering")
			}
		}
		if len(calls) == 1 {
			return "ATA", nil
		}
		return "AT+CHLD=2", nil
	}
	if len(calls) == 1 {
		return "AT+CHUP", nil
	}
	if target.State == Waiting {
		// 27.007：CHLD=1<idx> 释放的是 active/held 的那一路，等待中的通话
		// 要用 CHLD=0（UDUB，拒接等待来电），它不会挂断当前通话。
		return "AT+CHLD=0", nil
	}
	return fmt.Sprintf("AT+CHLD=1%d", target.Index), nil
}

// Established data/fax entries may coexist with voice. Pending data calls and
// unknown-mode entries stay visible: ATA could otherwise answer a data call.
func voiceControlCalls(calls []Call) []Call {
	var voice []Call
	for _, call := range calls {
		if !establishedDataCall(call) {
			voice = append(voice, call)
		}
	}
	return voice
}

func establishedDataCall(call Call) bool {
	return (call.Mode == 1 || call.Mode == 2) && call.State == Active
}

func sameCall(previous, current Call) bool {
	if (previous.State == Active || previous.State == Held) && current.State >= Dialing {
		return false
	}
	return previous.Index == current.Index && previous.Inbound == current.Inbound && previous.Mode == current.Mode &&
		(previous.Number == "" || current.Number == "" || previous.Number == current.Number)
}
