package parser

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type Result struct {
	Model         string
	InputTokens   int
	OutputTokens  int
	CacheCreation int
	CacheRead     int
	HasError      bool
}

type jsonResponse struct {
	Model string    `json:"model"`
	Usage jsonUsage `json:"usage"`
}

type jsonUsage struct {
	InputTokens   int `json:"input_tokens"`
	OutputTokens  int `json:"output_tokens"`
	CacheCreation int `json:"cache_creation_input_tokens"`
	CacheRead     int `json:"cache_read_input_tokens"`
}

func ParseJSON(body []byte) (Result, error) {
	var resp jsonResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Result{}, fmt.Errorf("parse json response: %w", err)
	}
	return Result{
		Model:         resp.Model,
		InputTokens:   resp.Usage.InputTokens,
		OutputTokens:  resp.Usage.OutputTokens,
		CacheCreation: resp.Usage.CacheCreation,
		CacheRead:     resp.Usage.CacheRead,
	}, nil
}

type sseMessageStart struct {
	Type    string `json:"type"`
	Message struct {
		Model string    `json:"model"`
		Usage jsonUsage `json:"usage"`
	} `json:"message"`
}

type deltaUsage struct {
	InputTokens   *int `json:"input_tokens,omitempty"`
	OutputTokens  *int `json:"output_tokens,omitempty"`
	CacheCreation *int `json:"cache_creation_input_tokens,omitempty"`
	CacheRead     *int `json:"cache_read_input_tokens,omitempty"`
}

type sseMessageDelta struct {
	Type  string       `json:"type"`
	Usage *deltaUsage  `json:"usage,omitempty"`
}

func ParseSSE(r io.Reader) (Result, error) {
	var result Result
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 256*1024), 2*1024*1024) // 2MB max line for extended thinking deltas
	var currentEvent string

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")

			switch currentEvent {
			case "message_start":
				var msg sseMessageStart
				if err := json.Unmarshal([]byte(data), &msg); err == nil {
					result.Model = msg.Message.Model
					result.InputTokens = msg.Message.Usage.InputTokens
					result.OutputTokens = msg.Message.Usage.OutputTokens
					result.CacheCreation = msg.Message.Usage.CacheCreation
					result.CacheRead = msg.Message.Usage.CacheRead
				}

			case "message_delta":
				var msg sseMessageDelta
				if err := json.Unmarshal([]byte(data), &msg); err == nil && msg.Usage != nil {
					if msg.Usage.InputTokens != nil {
						result.InputTokens = *msg.Usage.InputTokens
					}
					if msg.Usage.OutputTokens != nil {
						result.OutputTokens = *msg.Usage.OutputTokens
					}
					if msg.Usage.CacheCreation != nil {
						result.CacheCreation = *msg.Usage.CacheCreation
					}
					if msg.Usage.CacheRead != nil {
						result.CacheRead = *msg.Usage.CacheRead
					}
				}

			case "error":
				result.HasError = true
				return result, nil
			}
			currentEvent = ""
		}
	}
	return result, scanner.Err()
}
