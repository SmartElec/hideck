package modemvoice

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yibaiba/hideck/pkg/logger"
)

const (
	queryTimeout       = 3 * time.Second
	callCommandTimeout = 60 * time.Second
	// clccRetryDelay 只用于“列出通话但收尾报错”的半截响应，别把它当轮询间隔。
	clccRetryDelay = 250 * time.Millisecond
	// cpasStrikes 是 CLCC 仍列着通话、CPAS 却报空闲时，连续多少次才判定
	// 该通话其实已经释放。一次可能是 CLCC/CPAS 采样错位，两次就是真的残留。
	cpasStrikes = 2
)

// cpasGrace 给刚下发的 ATD/ATA 留出把 CPAS 推到“通话中”的时间。变量是为了
// 单测能把等待压到 0，生产路径不要改。
var cpasGrace = 1500 * time.Millisecond

// Executor is implemented by modem.Manager and shares its SMS/AT queue.
type Executor interface {
	ExecuteATContext(context.Context, string, time.Duration) (string, error)
}

type Client struct {
	executor Executor
	gate     chan struct{}

	// staleMu 保护 stale：按 CLCC 索引记录“CPAS 已空闲”的连续观测次数。
	staleMu sync.Mutex
	stale   map[int]staleVoice
}

// staleVoice 是一路 CLCC 残留通话的观测记录。
type staleVoice struct {
	since   time.Time
	strikes int
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

// CallsVerified 是给轮询循环用的快照：在 CLCC 之上再用 AT+CPAS 复核一遍。
// 控制路径（拨号/接听/挂断/DTMF）仍走 Calls，它们需要 CLCC 的原始结论。
func (c *Client) CallsVerified(ctx context.Context) ([]Call, error) {
	if err := c.lock(ctx); err != nil {
		return nil, err
	}
	defer func() { <-c.gate }()
	calls, err := c.calls(ctx)
	if err != nil {
		return nil, err
	}
	return c.dropReleasedByCPAS(ctx, calls), nil
}

// dropReleasedByCPAS 用 AT+CPAS 复核 CLCC 快照：EC20F 在对方挂断后偶尔仍把
// 该通话留在 +CLCC 里（状态停在 alerting/active），而 CPAS 已经回到 0（ready）。
// 只看 CLCC 的轮询永远等不到“通话消失”，界面就一直停在响铃/通话中。
//
// 判定要连续两轮且距首次发现超过 cpasGrace：ATD/ATA 刚下发时 CPAS 还没切到
// “通话中”，一次采样不足以定论；漏判只会晚一轮结束，误判会直接掐断通话。
func (c *Client) dropReleasedByCPAS(ctx context.Context, calls []Call) []Call {
	voice := make([]Call, 0, len(calls))
	for _, call := range calls {
		if call.Mode == 0 {
			voice = append(voice, call)
		}
	}
	if len(voice) == 0 {
		c.staleMu.Lock()
		c.stale = nil
		c.staleMu.Unlock()
		return calls
	}
	status, ok := c.phoneActivity(ctx)
	now := time.Now()
	c.staleMu.Lock()
	defer c.staleMu.Unlock()
	if !ok || status != cpasReady {
		// 查不到或者模组明确在通话中：本轮不做判断，观测记录清零重来。
		c.stale = nil
		return calls
	}
	if c.stale == nil {
		c.stale = make(map[int]staleVoice, len(voice))
	}
	next := make(map[int]staleVoice, len(voice))
	kept := make([]Call, 0, len(calls))
	for _, call := range calls {
		if call.Mode != 0 {
			kept = append(kept, call)
			continue
		}
		entry, seen := c.stale[call.Index]
		if !seen {
			entry = staleVoice{since: now}
		}
		entry.strikes++
		if entry.strikes >= cpasStrikes && now.Sub(entry.since) >= cpasGrace {
			logger.Warn("CLCC 残留通话已按 CPAS 判定释放", "clcc_index", call.Index,
				"number", call.Number, "state", int(call.State), "strikes", entry.strikes)
			continue
		}
		next[call.Index] = entry
		kept = append(kept, call)
	}
	c.stale = next
	return kept
}

// cpasReady 是 TS 27.007 里 AT+CPAS 的“ready”取值，其余取值都表示模组认为
// 自己还有活要干（拨号/响铃/通话中/休眠）。
const cpasReady = 0

// phoneActivity 读 AT+CPAS。失败不是错误：这条命令只用于复核，拿不到结果就
// 保持 CLCC 的结论。
func (c *Client) phoneActivity(ctx context.Context) (int, bool) {
	response, err := c.executor.ExecuteATContext(ctx, "AT+CPAS", queryTimeout)
	if err != nil {
		return 0, false
	}
	for _, raw := range strings.Split(strings.ReplaceAll(response, "\r", ""), "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "+CPAS:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "+CPAS:"))
		status, err := strconv.Atoi(value)
		if err != nil || status < 0 || status > 5 {
			return 0, false
		}
		return status, true
	}
	return 0, false
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
