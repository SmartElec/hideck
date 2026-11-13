package volte

import (
	"fmt"
	"strconv"
	"strings"
)

type LTEState struct {
	Registered bool
	Stat       int
	Raw        string
}

type PDPContext struct {
	CID    int
	PDP    string
	APN    string
	Active bool
}

func ParseCOPS(resp string) (mcc, mnc string, err error) {
	for _, raw := range strings.Split(resp, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(strings.ToUpper(line), "+COPS:") {
			continue
		}
		_, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		quoted, _, _, qok := cutQuoted(rest)
		if !qok {
			continue
		}
		digits := make([]rune, 0, len(quoted))
		for _, r := range quoted {
			if r >= '0' && r <= '9' {
				digits = append(digits, r)
			}
		}
		if len(digits) < 5 {
			continue
		}
		mcc = string(digits[:3])
		mnc = string(digits[3:])
		return mcc, mnc, nil
	}
	return "", "", fmt.Errorf("volte: missing +COPS PLMN in %q", compactAT(resp))
}

func ParseCEREG(resp string) (LTEState, error) {
	for _, raw := range strings.Split(resp, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(strings.ToUpper(line), "+CEREG:") {
			continue
		}
		_, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		parts := splitQCFGArgs(rest)
		if len(parts) < 1 {
			break
		}
		statIdx := 0
		if len(parts) > 1 {
			statIdx = 1
		}
		stat, err := strconv.Atoi(parts[statIdx])
		if err != nil {
			return LTEState{}, fmt.Errorf("volte: CEREG stat: %w", err)
		}
		return LTEState{Registered: stat == 1 || stat == 5, Stat: stat, Raw: line}, nil
	}
	return LTEState{}, fmt.Errorf("volte: missing +CEREG in %q", compactAT(resp))
}

// ParseCIREG 解析 AT+CIREG? 的 "+CIREG: <n>,<reg_info>"。reg_info 是位图，
// bit0 表示模块自身 IMS 已注册（EC2x 系列）；只有 1 个参数时按 reg_info 处理。
func ParseCIREG(resp string) (bool, error) {
	for _, raw := range strings.Split(resp, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(strings.ToUpper(line), "+CIREG:") {
			continue
		}
		_, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		parts := splitQCFGArgs(rest)
		idx := 0
		if len(parts) > 1 {
			idx = 1
		}
		if idx >= len(parts) {
			break
		}
		info, err := strconv.Atoi(parts[idx])
		if err != nil {
			return false, fmt.Errorf("volte: CIREG reg_info: %w", err)
		}
		return info&1 == 1, nil
	}
	return false, fmt.Errorf("volte: missing +CIREG in %q", compactAT(resp))
}

func ParseCGDCONT(resp string) ([]PDPContext, error) {
	var out []PDPContext
	for _, raw := range strings.Split(resp, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(strings.ToUpper(line), "+CGDCONT:") {
			continue
		}
		_, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		cidStr, pdp, apn, ok := parseCGDCONTArgs(rest)
		if !ok {
			continue
		}
		cid, err := strconv.Atoi(cidStr)
		if err != nil {
			continue
		}
		out = append(out, PDPContext{CID: cid, PDP: pdp, APN: apn})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("volte: missing +CGDCONT in %q", compactAT(resp))
	}
	return out, nil
}

func ParseCGACT(resp string) (map[int]bool, error) {
	out := map[int]bool{}
	found := false
	for _, raw := range strings.Split(resp, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(strings.ToUpper(line), "+CGACT:") {
			continue
		}
		_, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		parts := splitQCFGArgs(rest)
		if len(parts) < 2 {
			continue
		}
		cid, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		out[cid] = parts[1] == "1"
		found = true
	}
	if !found {
		return nil, fmt.Errorf("volte: missing +CGACT in %q", compactAT(resp))
	}
	return out, nil
}

func IMSContext(contexts []PDPContext) (PDPContext, bool) {
	for _, ctx := range contexts {
		if strings.Contains(strings.ToLower(ctx.APN), "ims") {
			return ctx, true
		}
	}
	return PDPContext{}, false
}

func parseCGDCONTArgs(rest string) (cid, pdp, apn string, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(rest), ",", 4)
	if len(parts) < 3 {
		return "", "", "", false
	}
	cid = strings.TrimSpace(parts[0])
	pdp = strings.Trim(strings.TrimSpace(parts[1]), `"`)
	apn = strings.Trim(strings.TrimSpace(parts[2]), `"`)
	return cid, pdp, apn, cid != ""
}

func COPSQueryCommand() string         { return "AT+COPS?" }
func COPSNumericFormatCommand() string { return "AT+COPS=3,2" }
func CEREGQueryCommand() string        { return "AT+CEREG?" }
func CIREGQueryCommand() string        { return "AT+CIREG?" }
func CGDCONTQueryCommand() string      { return "AT+CGDCONT?" }
func CGACTQueryCommand() string        { return "AT+CGACT?" }

func CGACTSetCommand(cid int, active bool) string {
	bit := 0
	if active {
		bit = 1
	}
	return fmt.Sprintf("AT+CGACT=%d,%d", bit, cid)
}
