package matchi

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Endpoint는 모델 하나를 부르는 곳이다. 판정 모델은 URL이 비면 이 컴퓨터의 모델 worker다. Name은 결과 파일에 남기는 짧은 이름이고,
// Key가 비면 인증 헤더를 보내지 않는다. OutputLimit은 쓰기 호출 하나의 출력 token 상한이고, 0이면 정하지 않은 것이다.
type Endpoint struct {
	URL, Model, Name, Key string
	OutputLimit           int
}

const (
	// localModel은 모델 worker가 올리는 판정 모델이다.
	localModel = "d3-ultra-INT4"
	// raonURL과 raonModel은 이 컴퓨터의 Raon 생성 서버(raon/serve.sh)다.
	raonURL   = "http://127.0.0.1:18080/v1"
	raonModel = "raon-r9700"
)

// Decision은 판정 모델의 연결이다. 환경 변수가 없으면 모델 worker이고, DLLIO_DECISION_URL·DLLIO_DECISION_MODEL·DLLIO_DECISION_KEY로
// 다른 곳을 정한다. URL은 서버 주소이고 요청은 그 아래 /v1/systemone으로 간다.
func Decision() (Endpoint, error) {
	return endpoint("DLLIO_DECISION")
}

// Language는 언어 모델의 연결이다. 환경 변수가 없으면 이 컴퓨터의 Raon 생성 서버이고, DLLIO_LANGUAGE_URL·DLLIO_LANGUAGE_MODEL·
// DLLIO_LANGUAGE_KEY로 다른 곳을 정한다. URL은 API의 바탕 주소(…/v1)이고 요청은 그 아래 /responses로 간다.
// DLLIO_LANGUAGE_MAX_OUTPUT은 호출 하나의 출력 token 상한이다.
func Language() (Endpoint, error) {
	out, err := endpoint("DLLIO_LANGUAGE")
	if err != nil {
		return out, err
	}
	if out.URL == "" {
		out.URL, out.Model = raonURL, raonModel
	}
	out.Name = out.Model[strings.LastIndex(out.Model, "/")+1:]
	if limit := value("DLLIO_LANGUAGE_MAX_OUTPUT", ""); limit != "" {
		out.OutputLimit, err = strconv.Atoi(limit)
		if err != nil || out.OutputLimit < 1 {
			return out, fmt.Errorf("DLLIO_LANGUAGE_MAX_OUTPUT은 양의 정수로 지정한다")
		}
	}
	return out, nil
}

// endpoint는 <prefix>_URL이 있으면 그 주소를, 없으면 모델 worker를 쓴다. 키는 <prefix>_KEY의 값, <prefix>_KEY_FILE이 가리키는 파일
// 순서로 찾고, 주지 않으면 인증 없이 부른다. 결과에 남기는 이름은 모델 이름의 마지막 '/' 뒤다.
func endpoint(prefix string) (Endpoint, error) {
	out := Endpoint{URL: strings.TrimRight(value(prefix+"_URL", ""), "/"), Model: localModel}
	if out.URL != "" {
		out.Model = value(prefix+"_MODEL", localModel)
	}
	out.Name = out.Model[strings.LastIndex(out.Model, "/")+1:]
	key, _, err := given(prefix + "_KEY")
	out.Key = key
	return out, err
}

func value(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// given은 환경 변수 name의 값이나 name_FILE이 가리키는 파일의 내용이다.
func given(name string) (string, bool, error) {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v, true, nil
	}
	if path := strings.TrimSpace(os.Getenv(name + "_FILE")); path != "" {
		key, err := readKey(path, name)
		return key, err == nil, err
	}
	return "", false, nil
}

func readKey(path, name string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s 키 파일을 읽지 못했다", name)
	}
	return strings.TrimSpace(string(data)), nil
}

var keyFiles = map[string]string{"VWORLD_API_KEY": "vworld.txt"}

// Secret은 환경 변수의 값, 환경 변수 NAME_FILE이 가리키는 파일, secrets/의 정해진 파일 순서로 키를 찾는다.
func Secret(root, name string) (string, error) {
	key, found, err := given(name)
	if found || err != nil {
		return key, err
	}
	file, ok := keyFiles[name]
	if !ok {
		return "", fmt.Errorf("%s 또는 %s_FILE을 지정한다", name, name)
	}
	return readKey(filepath.Join(root, "secrets", file), name)
}
