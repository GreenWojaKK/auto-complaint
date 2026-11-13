package matchi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"dllio/backend/jsonx"
)

type scopeKey struct{}

// Scope는 호출 기록에 남길 민원과 단계다.
type Scope struct {
	Case  string `json:"case,omitempty"`
	Phase string `json:"phase,omitempty"`
}

// WithScope는 이 맥락의 호출에 민원과 단계를 붙인다.
func WithScope(ctx context.Context, caseID, phase string) context.Context {
	return context.WithValue(ctx, scopeKey{}, Scope{caseID, phase})
}

// Usage는 호출 하나의 토큰 사용량이다.
type Usage struct {
	Input      int `json:"input_tokens"`
	CacheRead  int `json:"cache_read_tokens"`
	CacheWrite int `json:"cache_write_tokens"`
	Output     int `json:"output_tokens"`
	Reasoning  int `json:"output_reasoning_tokens"`
}

// Part는 메시지의 한 조각이다.
type Part struct {
	Kind    string `json:"part_kind"`
	Content string `json:"content"`
}

// Message는 실행 기록에 남기는 요청이나 응답이다. Kind는 request 또는 response다. 검토 도구가 호출과 다시 쓰기 횟수를 여기서 센다.
type Message struct {
	Kind         string `json:"kind"`
	Instructions string `json:"instructions,omitempty"`
	ModelName    string `json:"model_name,omitempty"`
	ResponseID   string `json:"provider_response_id,omitempty"`
	Usage        *Usage `json:"usage,omitempty"`
	Timestamp    string `json:"timestamp"`
	Parts        []Part `json:"parts"`
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

type callRecord struct {
	Scope      Scope    `json:"scope"`
	Model      string   `json:"model"`
	StartedAt  string   `json:"startedAt"`
	Settings   any      `json:"settings"`
	Parameters struct{} `json:"requestParameters"`
	Hash       string   `json:"requestHash"`
	Messages   int      `json:"messageCount"`
	Request    any      `json:"request"`
	Status     string   `json:"status"`
	Response   *Message `json:"response,omitempty"`
	Error      string   `json:"error,omitempty"`
	FinishedAt string   `json:"finishedAt,omitempty"`
}

// Logger는 호출 하나마다 파일 하나로 요청과 응답, 사용량을 남긴다. test/review/audit_usage.py가 읽는다.
type Logger struct{ Dir string }

// NewLogger는 DLLIO_CALL_DIR, 없으면 산출물 폴더의 model-calls/dllio에 쓴다.
func NewLogger(artifacts string) *Logger {
	dir := os.Getenv("DLLIO_CALL_DIR")
	if dir == "" {
		dir = filepath.Join(artifacts, "model-calls", "dllio")
	}
	return &Logger{dir}
}

type call struct {
	path   string
	record callRecord
}

func (l *Logger) begin(ctx context.Context, model string, settings, request any, body []byte, messages int) (*call, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	scope, _ := ctx.Value(scopeKey{}).(Scope)
	c := &call{filepath.Join(l.Dir, hex.EncodeToString(id)+".json"), callRecord{
		Scope: scope, Model: model, StartedAt: now(), Settings: settings,
		Hash: jsonx.SHA(string(body)), Messages: messages, Request: request, Status: "pending"}}
	return c, jsonx.WriteFile(c.path, c.record)
}

func (c *call) finish(response *Message, failure error) error {
	c.record.FinishedAt = now()
	if failure != nil {
		c.record.Status, c.record.Error = "failed", failure.Error()
	} else {
		c.record.Status, c.record.Response = "complete", response
	}
	return jsonx.WriteFile(c.path, c.record)
}

// CreditExhausted는 API 잔액이 바닥나 실행을 멈춰야 함을 뜻한다.
type CreditExhausted struct{ Message string }

func (e *CreditExhausted) Error() string { return "API 크레딧 소진: " + e.Message }

var (
	rateLimit = regexp.MustCompile(`(?i)rate limit`)
	billing   = regexp.MustCompile(`(?i)credit|balance|billing|funds`)
	depleted  = regexp.MustCompile(`(?i)insufficient|exhaust|deplet|too low|not enough|exceeded|payment required`)
)

// CheckCredit은 잔액 소진이면 CreditExhausted를 돌려준다. 분당 호출 제한은 잔액 문제가 아니다.
func CheckCredit(message string, status int) error {
	if rateLimit.MatchString(message) {
		return nil
	}
	if status == 402 || (billing.MatchString(message) && depleted.MatchString(message)) {
		if len(message) > 600 {
			message = message[:600]
		}
		return &CreditExhausted{message}
	}
	return nil
}
