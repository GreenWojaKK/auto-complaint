// Package matchi는 판정 모델과 언어 모델을 부르고 호출을 기록한다. 도구 호출 루프 없이, 코드가 만든 문항과 출력 형식만 보낸다.
package matchi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"dllio/backend/jsonx"
)

// Option은 문항의 보기 하나와 그 기준이다.
type Option struct{ Key, Criterion string }

// Question은 판정 문항이다. Key는 호출한 쪽이 답을 찾는 이름이다.
type Question struct {
	Key     string
	Text    string
	Options []Option
}

// Ask는 문항의 목적과 판정 지시다.
type Ask struct {
	Goal, Instructions string
	Batch              int
}

// Local은 이 컴퓨터의 모델 worker다. 요청 본문을 그대로 넘기고, 같은 요청을 HTTP로 보냈을 때의 응답 본문을 돌려준다.
type Local interface {
	Call(ctx context.Context, op string, body []byte) ([]byte, error)
}

// Decider는 System One 형식의 판정 요청을 보낸다. Local이 있으면 모델 worker에, 없으면 URL의 판정 API에 보낸다.
// Key가 비면 인증 헤더를 보내지 않는다.
type Decider struct {
	URL, Model, Key string
	Local           Local
	Log             *Logger
	Client          *http.Client
}

// Distribution은 문항마다 보기별 확률이다.
type Distribution map[string]map[string]float64

type answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type decisionReply struct {
	Model   string            `json:"model"`
	Answers map[string]answer `json:"answers"`
	Usage   struct {
		Input  int `json:"input_tokens"`
		Output int `json:"output_tokens"`
	} `json:"usage"`
}

// Decide는 문항을 Batch개씩 나눠 함께 보내고, 문항마다 보기별 확률을 돌려준다.
func (d *Decider) Decide(ctx context.Context, state string, questions []Question, ask Ask) (Distribution, []Message, error) {
	if ask.Batch <= 0 {
		ask.Batch = len(questions)
	}
	type outcome struct {
		answers  Distribution
		messages []Message
		err      error
	}
	var parts [][]Question
	for i := 0; i < len(questions); i += ask.Batch {
		parts = append(parts, questions[i:min(i+ask.Batch, len(questions))])
	}
	results := make([]outcome, len(parts))
	var wait sync.WaitGroup
	for i, part := range parts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results[i].answers, results[i].messages, results[i].err = d.fitted(ctx, state, part, ask)
		}()
	}
	wait.Wait()
	out, messages := Distribution{}, []Message{}
	for _, r := range results {
		if r.err != nil {
			return nil, nil, r.err
		}
		for k, v := range r.answers {
			out[k] = v
		}
		messages = append(messages, r.messages...)
	}
	return out, messages, nil
}

// fitted는 묶음 하나를 묻는다. 요청이 판정 모델의 입력 한도를 넘으면 묶음을 반으로 나눠 다시 묻는다.
func (d *Decider) fitted(ctx context.Context, state string, part []Question, ask Ask) (Distribution, []Message, error) {
	answers, messages, err := d.one(ctx, state, part, ask)
	if err == nil || len(part) < 2 || !strings.Contains(err.Error(), "request_too_long") {
		return answers, messages, err
	}
	out, all := Distribution{}, []Message{}
	for _, half := range [][]Question{part[:len(part)/2], part[len(part)/2:]} {
		answers, messages, err := d.fitted(ctx, state, half, ask)
		if err != nil {
			return nil, nil, err
		}
		for k, v := range answers {
			out[k] = v
		}
		all = append(all, messages...)
	}
	return out, all, nil
}

func (d *Decider) one(ctx context.Context, state string, part []Question, ask Ask) (Distribution, []Message, error) {
	questions := jsonx.Object{}
	for i, q := range part {
		name := fmt.Sprintf("q%d", i)
		criteria := jsonx.Object{}
		for _, o := range q.Options {
			criteria = append(criteria, jsonx.Field{Key: o.Key, Value: o.Criterion})
		}
		questions = append(questions, jsonx.Field{Key: name, Value: jsonx.Object{
			{Key: "type", Value: "choice"},
			{Key: "criteria", Value: criteria},
			{Key: "instructions", Value: jsonx.Object{
				{Key: "field", Value: name}, {Key: "question", Value: q.Text}, {Key: "goal", Value: ask.Goal}, {Key: "background", Value: ask.Instructions}}},
		}})
	}
	request := jsonx.Object{{Key: "state", Value: state}, {Key: "model", Value: d.Model}, {Key: "questions", Value: questions}}
	body, err := jsonx.Marshal(request)
	if err != nil {
		return nil, nil, err
	}
	asked := Message{Kind: "request", Timestamp: now(), Parts: []Part{{"user-prompt", string(body)}}}
	record, err := d.Log.begin(ctx, d.Model, nil, request, body, 1)
	if err != nil {
		return nil, nil, err
	}
	raw, reply, err := d.post(ctx, body)
	if err == nil {
		err = checkReply(reply, part)
	}
	if err != nil {
		if logErr := record.finish(nil, err); logErr != nil {
			return nil, nil, logErr
		}
		return nil, nil, err
	}
	response := Message{Kind: "response", ModelName: reply.Model, Timestamp: now(), Parts: []Part{{"text", string(raw)}},
		Usage: &Usage{Input: reply.Usage.Input, Output: reply.Usage.Output}}
	if err := record.finish(&response, nil); err != nil {
		return nil, nil, err
	}
	out := Distribution{}
	for i, q := range part {
		out[q.Key] = reply.Answers[fmt.Sprintf("q%d", i)].Probabilities
	}
	return out, []Message{asked, response}, nil
}

// postWaits는 판정 API에 연결하지 못했거나 서버 오류(5xx)를 받았을 때 다시 보내기 전에 기다리는 시간이다.
var postWaits = []time.Duration{5 * time.Second, 20 * time.Second, 60 * time.Second}

func (d *Decider) post(ctx context.Context, body []byte) ([]byte, *decisionReply, error) {
	if d.Local != nil {
		raw, err := d.Local.Call(ctx, "decide", body)
		if err != nil {
			return nil, nil, fmt.Errorf("모델 worker의 판정: %w", err)
		}
		var reply decisionReply
		if err := json.Unmarshal(raw, &reply); err != nil {
			return nil, nil, fmt.Errorf("모델 worker의 판정 응답을 읽지 못했다: %w", err)
		}
		return raw, &reply, nil
	}
	client := d.Client
	if client == nil {
		client = http.DefaultClient
	}
	var response *http.Response
	var raw []byte
	for attempt := 0; ; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL+"/v1/systemone", bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		if d.Key != "" {
			request.Header.Set("Authorization", "Bearer "+d.Key)
		}
		response, err = client.Do(request)
		if err == nil {
			raw, err = io.ReadAll(response.Body)
			response.Body.Close()
		}
		retry := err != nil || response.StatusCode >= 500
		if !retry || attempt >= len(postWaits) || ctx.Err() != nil {
			if err != nil {
				return nil, nil, err
			}
			break
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(postWaits[attempt]):
		}
	}
	if response.StatusCode >= 400 {
		if err := CheckCredit(string(raw), response.StatusCode); err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("판정 API HTTP %d: %.400s", response.StatusCode, raw)
	}
	var reply decisionReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, nil, fmt.Errorf("판정 API 응답을 읽지 못했다: %w", err)
	}
	return raw, &reply, nil
}

// checkReply는 응답이 문항과 맞는지 본다. 답의 이름, 고른 보기, 보기별 확률의 범위와 합을 검사한다.
func checkReply(reply *decisionReply, part []Question) error {
	if len(reply.Answers) != len(part) {
		return fmt.Errorf("판정 API 응답: 문항 %d개에 답 %d개", len(part), len(reply.Answers))
	}
	for i, q := range part {
		name := fmt.Sprintf("q%d", i)
		a, ok := reply.Answers[name]
		if !ok || a.Type != "choice" {
			return fmt.Errorf("판정 API 응답: %s의 답이 없거나 형식이 다르다", name)
		}
		if len(a.Probabilities) != len(q.Options) {
			return fmt.Errorf("판정 API 응답: %s의 보기 수가 다르다", name)
		}
		total, chosen := 0.0, false
		for _, o := range q.Options {
			p, ok := a.Probabilities[o.Key]
			if !ok || math.IsNaN(p) || p < 0 || p > 1 {
				return fmt.Errorf("판정 API 응답: %s의 %s 확률이 잘못됐다", name, o.Key)
			}
			total += p
			chosen = chosen || a.Choice == o.Key
		}
		if !chosen || math.Abs(total-1) > 1e-6+float64(len(q.Options))*0.005 {
			return fmt.Errorf("판정 API 응답: %s의 선택이나 확률 합이 잘못됐다", name)
		}
	}
	return nil
}
