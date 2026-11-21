package matchi

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dllio/backend/jsonx"
)

const (
	outputRetries = 3
	httpRetries   = 2
	maxOutput     = 24000
)

// Writer는 Responses API로 언어 모델을 부른다. MaxOutput은 호출 하나의 출력 token 상한이고, 0이면 24000이다.
// Fresh이면 검사를 통과하지 못한 답을 대화에 잇지 않고, 처음 입력에 고칠 내용을 붙인 새 요청으로 다시 쓰게 한다.
// 앞선 답을 대화에서 본 모델이 그 답을 그대로 되풀이할 때 쓴다.
type Writer struct {
	URL, Model, Effort, Key string
	MaxOutput               int
	Fresh                   bool
	Log                     *Logger
	Client                  *http.Client
}

// Spec은 구조화 호출 하나의 입력과 검사다. Check는 고칠 내용을 글로 돌려주고, 문제가 없으면 빈 글을 돌려준다.
// Retries는 다시 쓰게 하는 최대 횟수이고, 0이면 3번이다.
type Spec[T any] struct {
	Name, Instructions, Input string
	Enums                     map[string][]string
	Limits                    map[string]Limit
	Check                     func(*T) string
	Retries                   int
}

type responsesReply struct {
	ID     string            `json:"id"`
	Status string            `json:"status"`
	Model  string            `json:"model"`
	Output []json.RawMessage `json:"output"`
	Usage  struct {
		Input   int `json:"input_tokens"`
		Output  int `json:"output_tokens"`
		Details struct {
			Cached int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		OutputDetails struct {
			Reasoning int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

// Write는 출력 형식과 검사를 통과한 값을 돌려준다. 통과하지 못하면 어긋난 곳을 알려 다시 쓰게 한다. 검사 통과 전까지의 응답도 messages에 남는다.
func Write[T any](ctx context.Context, w *Writer, spec Spec[T]) (*T, []Message, error) {
	schema, err := SchemaOf[T](spec.Enums, spec.Limits)
	if err != nil {
		return nil, nil, err
	}
	format := jsonx.Object{{Key: "type", Value: "json_schema"}, {Key: "name", Value: spec.Name}, {Key: "schema", Value: schema}, {Key: "strict", Value: true}}
	instructions := strings.TrimSpace(spec.Instructions)
	input := []any{jsonx.Object{{Key: "role", Value: "user"}, {Key: "content", Value: spec.Input}}}
	messages := []Message{{Kind: "request", Instructions: instructions, Timestamp: now(), Parts: []Part{{"user-prompt", spec.Input}}}}
	previous := ""
	for attempt := 0; ; attempt++ {
		body := jsonx.Object{
			{Key: "model", Value: w.Model}, {Key: "stream", Value: false}, {Key: "include", Value: []string{"reasoning.encrypted_content"}},
			{Key: "input", Value: input}, {Key: "instructions", Value: instructions}, {Key: "max_output_tokens", Value: cmp.Or(w.MaxOutput, maxOutput)}}
		// 재요청에는 직전 응답 id를 붙여, 앞부분을 캐시에서 다시 읽었는지 진단을 받는다. 진단은 요금이 없다.
		if strings.HasPrefix(previous, "resp_") {
			body = append(body, jsonx.Field{Key: "prompt_cache_options", Value: jsonx.Object{{Key: "comparison_response_id", Value: previous}}})
		}
		body = append(body,
			jsonx.Field{Key: "reasoning", Value: jsonx.Object{{Key: "effort", Value: w.Effort}, {Key: "context", Value: "all_turns"}}},
			jsonx.Field{Key: "text", Value: jsonx.Object{{Key: "format", Value: format}}})
		reply, text, response, err := w.send(ctx, body, len(input))
		if err != nil {
			return nil, messages, err
		}
		previous = reply.ID
		messages = append(messages, *response)
		var out *T
		var feedback string
		switch {
		// 추론에 상한을 다 쓴 답은 형식 오류가 아니다. 같은 요청을 다시 보내지 않고 호출을 끝낸다.
		case reply.Status == "incomplete" && (!w.Fresh || reply.Usage.OutputDetails.Reasoning > 0):
			return nil, messages, fmt.Errorf("언어 모델의 답이 출력 상한 %d token에서 끝나지 않았다(추론 %d token)", reply.Usage.Output, reply.Usage.OutputDetails.Reasoning)
		// 추론 없이 본문이 상한까지 이어진 답은 같은 내용을 되풀이한 것이다. 줄여 쓰라는 고칠 내용을 붙여 새 요청으로 다시 쓰게 한다.
		case reply.Status == "incomplete":
			feedback = fmt.Sprintf("출력이 상한 %d token에서 끝나지 않았다. 같은 항목이나 문장을 되풀이하지 않고, 문장과 목록을 질문에 필요한 만큼으로 줄여 다시 쓴다.", reply.Usage.Output)
		default:
			if out, feedback, err = parseOutput[T](text, schema); err != nil {
				return nil, messages, err
			}
			if feedback == "" && spec.Check != nil {
				feedback = spec.Check(out)
			}
		}
		if feedback == "" {
			return out, messages, nil
		}
		if retries := cmp.Or(spec.Retries, outputRetries); attempt == retries {
			return nil, messages, fmt.Errorf("출력 검사를 %d번 통과하지 못했다: %.600s", attempt+1, feedback)
		}
		if w.Fresh {
			input = []any{jsonx.Object{{Key: "role", Value: "user"}, {Key: "content", Value: spec.Input + "\n\n같은 요청을 처음부터 다시 작성한다. 앞서 작성한 답은 다음 검사를 통과하지 못했다.\n" + feedback}}}
		} else {
			prompt := "Validation feedback:\n" + feedback + "\n\nFix the errors and try again."
			for _, item := range reply.Output {
				input = append(input, item)
			}
			input = append(input, jsonx.Object{{Key: "role", Value: "user"}, {Key: "content", Value: []any{jsonx.Object{{Key: "type", Value: "input_text"}, {Key: "text", Value: prompt}}}}})
		}
		messages = append(messages, Message{Kind: "request", Timestamp: now(), Parts: []Part{{"retry-prompt", feedback}}})
	}
}

func parseOutput[T any](text string, schema jsonx.Object) (*T, string, error) {
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return nil, "출력이 JSON이 아니다: " + err.Error(), nil
	}
	found, err := validate(schema, value)
	if err != nil {
		return nil, "", err
	}
	if len(found) > 0 {
		return nil, strings.Join(found, "; "), nil
	}
	var out T
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, "", err
	}
	return &out, "", nil
}

func (w *Writer) send(ctx context.Context, body jsonx.Object, inputs int) (*responsesReply, string, *Message, error) {
	data, err := jsonx.Marshal(body)
	if err != nil {
		return nil, "", nil, err
	}
	record, err := w.Log.begin(ctx, w.Model, jsonx.Object{{Key: "effort", Value: w.Effort}, {Key: "max_tokens", Value: cmp.Or(w.MaxOutput, maxOutput)}}, body, data, inputs)
	if err != nil {
		return nil, "", nil, err
	}
	reply, text, err := w.post(ctx, data)
	if err != nil {
		if logErr := record.finish(nil, err); logErr != nil {
			return nil, "", nil, logErr
		}
		return nil, "", nil, err
	}
	response := &Message{Kind: "response", ModelName: reply.Model, ResponseID: reply.ID, Timestamp: now(), Parts: []Part{{"text", text}},
		Usage: &Usage{Input: reply.Usage.Input, CacheRead: reply.Usage.Details.Cached, Output: reply.Usage.Output, Reasoning: reply.Usage.OutputDetails.Reasoning}}
	if err := record.finish(response, nil); err != nil {
		return nil, "", nil, err
	}
	return reply, text, response, nil
}

// post는 연결 실패와 일시적인 HTTP 오류를 두 번까지 다시 보낸다. 잔액 소진은 다시 보내지 않는다. 요청 시간 제한은 두지 않는다.
func (w *Writer) post(ctx context.Context, data []byte) (*responsesReply, string, error) {
	client := w.Client
	if client == nil {
		client = http.DefaultClient
	}
	for attempt := 0; ; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL+"/responses", bytes.NewReader(data))
		if err != nil {
			return nil, "", err
		}
		request.Header.Set("Content-Type", "application/json")
		if w.Key != "" {
			request.Header.Set("Authorization", "Bearer "+w.Key)
		}
		response, err := client.Do(request)
		var raw []byte
		status, wait, again := 0, backoff(attempt), err != nil
		if err == nil {
			raw, err = io.ReadAll(response.Body)
			response.Body.Close()
			status = response.StatusCode
			if err != nil {
				again = true
			} else if status >= 400 {
				err = fmt.Errorf("언어 모델 API HTTP %d: %.600s", status, raw)
				if creditErr := CheckCredit(string(raw), status); creditErr != nil {
					return nil, "", creditErr
				}
				again = retryable(response)
				wait = waitOf(response, wait)
			}
		}
		if err == nil {
			return readReply(raw)
		}
		if !again || attempt == httpRetries {
			return nil, "", err
		}
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(wait):
		}
	}
}

func retryable(r *http.Response) bool {
	switch r.Header.Get("x-should-retry") {
	case "true":
		return true
	case "false":
		return false
	}
	return r.StatusCode == 408 || r.StatusCode == 409 || r.StatusCode == 429 || r.StatusCode >= 500
}

func backoff(attempt int) time.Duration {
	return min(time.Duration(500<<attempt)*time.Millisecond, 8*time.Second)
}

func waitOf(r *http.Response, fallback time.Duration) time.Duration {
	if ms, err := strconv.ParseFloat(r.Header.Get("retry-after-ms"), 64); err == nil && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	if s, err := strconv.ParseFloat(r.Header.Get("retry-after"), 64); err == nil && s > 0 && s <= 60 {
		return time.Duration(s * float64(time.Second))
	}
	return fallback
}

func readReply(raw []byte) (*responsesReply, string, error) {
	var reply responsesReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, "", fmt.Errorf("언어 모델 응답을 읽지 못했다: %w", err)
	}
	var text strings.Builder
	for _, item := range reply.Output {
		var message struct {
			Type    string `json:"type"`
			Content []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
		}
		if err := json.Unmarshal(item, &message); err != nil {
			return nil, "", fmt.Errorf("언어 모델 응답을 읽지 못했다: %w", err)
		}
		if message.Type != "message" {
			continue
		}
		for _, content := range message.Content {
			if content.Type == "refusal" {
				return nil, "", errors.New("언어 모델이 응답을 거절했다: " + content.Refusal)
			}
			text.WriteString(content.Text)
		}
	}
	return &reply, text.String(), nil
}
