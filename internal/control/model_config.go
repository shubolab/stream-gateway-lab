package control

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// 页面只编辑连接参数；其他模型预算、reasoning 等配置原样保留。
func (s *Server) readModelConfig() (map[string]json.RawMessage, error) {
	if s.config.ModelConfig == "" {
		return nil, errors.New("model configuration path is not configured")
	}
	data, err := os.ReadFile(s.config.ModelConfig)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{"base_url": json.RawMessage(`"https://api.openai.com/v1"`)}, nil
	}
	if err != nil {
		return nil, err
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	if config == nil {
		return nil, errors.New("model configuration must be an object")
	}
	return config, nil
}

func modelString(config map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(config[key], &value)
	return value
}

func modelConfigView(config map[string]json.RawMessage) gin.H {
	return gin.H{
		"base_url":    modelString(config, "base_url"),
		"model":       modelString(config, "model"),
		"api_key_set": strings.TrimSpace(modelString(config, "api_key")) != "",
	}
}

func (s *Server) getModelConfig(c *gin.Context) {
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	config, err := s.readModelConfig()
	if err != nil {
		fail(c, 500, "无法读取模型配置，请检查服务端配置文件")
		return
	}
	c.JSON(200, modelConfigView(config))
}

func (s *Server) setModelConfig(c *gin.Context) {
	var input struct {
		BaseURL string `json:"base_url"`
		Model   string `json:"model"`
		APIKey  string `json:"api_key"`
	}
	if !readJSON(c, &input) {
		return
	}
	input.BaseURL = strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
	input.Model, input.APIKey = strings.TrimSpace(input.Model), strings.TrimSpace(input.APIKey)
	base, err := url.Parse(input.BaseURL)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || strings.HasSuffix(base.Path, "/responses") || len(input.BaseURL) > 2048 {
		fail(c, 400, "请输入 HTTP(S) API 基址，例如 https://api.openai.com/v1，不要追加 /responses")
		return
	}
	if input.Model == "" || len(input.Model) > 256 || len(input.APIKey) > 4096 {
		fail(c, 400, "请填写有效的模型名称和 API 密钥")
		return
	}
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	config, err := s.readModelConfig()
	if err != nil {
		fail(c, 500, "无法读取模型配置，请检查服务端配置文件")
		return
	}
	if input.APIKey == "" {
		input.APIKey = strings.TrimSpace(modelString(config, "api_key"))
	}
	if input.APIKey == "" {
		fail(c, 400, "首次配置请填写 API 密钥")
		return
	}
	for key, value := range map[string]string{"base_url": input.BaseURL, "model": input.Model, "api_key": input.APIKey} {
		config[key], _ = json.Marshal(value)
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil || WritePrivateFile(s.config.ModelConfig, data) != nil {
		fail(c, 500, "无法保存模型配置，请检查服务端文件权限")
		return
	}
	c.JSON(200, modelConfigView(config))
}
