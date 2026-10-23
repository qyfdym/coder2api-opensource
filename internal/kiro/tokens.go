package kiro

// token 估算与上下文窗口（移植自 Kiro-Go proxy/token_estimator.go 与 kiro.go）。
//
// Kiro 的流里没有可靠的 token 计数：输入 token 用 contextUsageEvent 的占用百分比 × 模型
// 上下文窗口换算（拿不到时按请求内容估算），输出 token 按输出内容估算。

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
)

func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	runes := []rune(text)
	n := len(runes)
	if n < 5 {
		return max(1, int(math.Ceil(float64(n)/3.0)))
	}
	var ascii, digits, symbols, nonASCII int
	for _, r := range runes {
		switch {
		case r >= 0x80:
			nonASCII++
		case r >= '0' && r <= '9':
			digits++
		case (r >= '!' && r <= '/') || (r >= ':' && r <= '@') || (r >= '[' && r <= '`') || (r >= '{' && r <= '~'):
			symbols++
		default:
			ascii++
		}
	}
	est := int(math.Ceil(float64(ascii)/4.5 + float64(digits)/2.0 + float64(symbols)/1.5 + float64(nonASCII)/1.5))
	if est < 1 {
		return 1
	}
	return est
}

func estimateJSONTokens(v any) int {
	if v == nil {
		return 0
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return estimateTokens(string(raw))
}

// estimateRequestTokens OpenAI 请求的输入 token 估算（contextUsageEvent 缺失时的兜底）。
func estimateRequestTokens(req *openAIRequest) int {
	if req == nil {
		return 0
	}
	total := 0
	for _, m := range req.Messages {
		switch c := m.Content.(type) {
		case nil:
		case string:
			total += estimateTokens(c)
		default:
			if t := extractMessageText(c); t != "" {
				total += estimateTokens(t)
			} else {
				total += estimateJSONTokens(c)
			}
		}
		total += estimateTokens(m.ToolCallID)
		for _, tc := range m.ToolCalls {
			total += estimateTokens(tc.Function.Name) + estimateTokens(tc.Function.Arguments)
		}
	}
	for _, t := range req.Tools {
		total += estimateTokens(t.Function.Name) + estimateTokens(t.Function.Description) + estimateJSONTokens(t.Function.Parameters)
	}
	return total
}

// contextWindow 模型上下文窗口：Claude 4.6 及以后（含 5.x）为 1M，4.5 及以前为 200K
// （与 ListAvailableModels 的口径一致）。用于把 contextUsagePercentage 换算成输入 token。
func contextWindow(model string) int {
	if isLargeContextModel(model) {
		return 1_000_000
	}
	return 200_000
}

var claudeVersionExtractor = regexp.MustCompile(`claude-(?:opus|sonnet|haiku)-(\d+)(?:[.-](\d+))?`)

func isLargeContextModel(model string) bool {
	m := strings.ToLower(model)
	if match := claudeVersionExtractor.FindStringSubmatch(m); match != nil {
		major, err := strconv.Atoi(match[1])
		if err == nil {
			if major > 4 {
				return true
			}
			minor := 0
			if match[2] != "" {
				p, err := strconv.Atoi(match[2])
				if err != nil {
					return false
				}
				minor = p
			}
			return major == 4 && minor >= 6
		}
	}
	for _, tag := range []string{"4.6", "4-6", "4.7", "4-7", "4.8", "4-8", "4.9", "4-9"} {
		if strings.Contains(m, tag) {
			return true
		}
	}
	return false
}

// extractThinkingFromContent 把正文里的 <thinking>…</thinking> 块拆出来（非流式 / 统计用）。
func extractThinkingFromContent(content string) (text, reasoning string) {
	result := content
	for {
		start := strings.Index(result, "<thinking>")
		if start < 0 {
			break
		}
		end := strings.Index(result[start:], "</thinking>")
		if end < 0 {
			break
		}
		end += start
		reasoning += result[start+10 : end]
		result = result[:start] + result[end+11:]
	}
	return strings.TrimSpace(result), reasoning
}
