package gateway

import (
	"fmt"
	"os"
)

type Binding struct {
	Pair             string `json:"pair"`
	BrainTokenEnv    string `json:"brain_token_env"`
	ExecutorTokenEnv string `json:"executor_token_env"`
}

func (b Binding) Token(role string) (string, error) {
	key := b.BrainTokenEnv
	if role == "executor" {
		key = b.ExecutorTokenEnv
	}
	token := os.Getenv(key)
	if token == "" {
		return "", fmt.Errorf("missing credential environment variable: %s", key)
	}
	return token, nil
}
