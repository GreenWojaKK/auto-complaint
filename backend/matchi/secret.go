package matchi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var keyFiles = map[string]string{"OPENAI_API_KEY": "oai-001.txt", "VLLM_SR_API_KEY": "vllm-001.txt", "VWORLD_API_KEY": "vworld.txt"}

// Secret은 환경 변수의 값, 환경 변수 NAME_FILE이 가리키는 파일, secrets/의 정해진 파일 순서로 키를 찾는다.
func Secret(root, name string) (string, error) {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value, nil
	}
	path := os.Getenv(name + "_FILE")
	if path == "" {
		file, ok := keyFiles[name]
		if !ok {
			return "", fmt.Errorf("%s 또는 %s_FILE을 지정한다", name, name)
		}
		path = filepath.Join(root, "secrets", file)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s 키 파일을 읽지 못했다", name)
	}
	return strings.TrimSpace(string(data)), nil
}
