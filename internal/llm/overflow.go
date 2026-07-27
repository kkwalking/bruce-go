package llm

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

type APIError struct {
	Provider   string
	StatusCode int
	Status     string
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s API request failed: HTTP %s\n%s", e.Provider, e.Status, e.Body)
}

type ContextOverflowError struct {
	Cause error
}

func (e *ContextOverflowError) Error() string {
	if e == nil || e.Cause == nil {
		return "context window overflow"
	}
	return e.Cause.Error()
}

func (e *ContextOverflowError) Unwrap() error { return e.Cause }

var overflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)prompt is too long`),
	regexp.MustCompile(`(?i)request_too_large`),
	regexp.MustCompile(`(?i)input is too long for requested model`),
	regexp.MustCompile(`(?i)exceeds the context window`),
	regexp.MustCompile(`(?i)exceeds (?:the )?(?:model'?s )?maximum context length(?: of [\d,]+ tokens?|\s*\([\d,]+\))`),
	regexp.MustCompile(`(?i)input token count.*exceeds the maximum`),
	regexp.MustCompile(`(?i)maximum prompt length is \d+`),
	regexp.MustCompile(`(?i)reduce the length of the messages`),
	regexp.MustCompile(`(?i)maximum context length is \d+ tokens`),
	regexp.MustCompile(`(?i)exceeds (?:the )?maximum allowed input length of [\d,]+ tokens?`),
	regexp.MustCompile(`(?i)input \(\d+ tokens\) is longer than the model'?s context length \(\d+ tokens\)`),
	regexp.MustCompile(`(?i)exceeds the limit of \d+`),
	regexp.MustCompile(`(?i)exceeds the available context size`),
	regexp.MustCompile(`(?i)greater than the context length`),
	regexp.MustCompile(`(?i)context window exceeds limit`),
	regexp.MustCompile(`(?i)exceeded model token limit`),
	regexp.MustCompile(`(?i)too large for model with \d+ maximum context length`),
	regexp.MustCompile(`(?i)model_context_window_exceeded`),
	regexp.MustCompile(`(?i)prompt too long; exceeded (?:max )?context length`),
	regexp.MustCompile(`(?i)context[_ ]length[_ ]exceeded`),
	regexp.MustCompile(`(?i)too many tokens`),
	regexp.MustCompile(`(?i)token limit exceeded`),
	regexp.MustCompile(`(?i)^4(?:00|13)\s*(?:status code)?\s*\(no body\)`),
}

var nonOverflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(Throttling error|Service unavailable):`),
	regexp.MustCompile(`(?i)rate limit`),
	regexp.MustCompile(`(?i)too many requests`),
}

func IsContextOverflowError(err error) bool {
	if err == nil {
		return false
	}
	var overflow *ContextOverflowError
	if errors.As(err, &overflow) {
		return true
	}
	var apiError *APIError
	if errors.As(err, &apiError) && (apiError.StatusCode == 400 || apiError.StatusCode == 413) && strings.TrimSpace(apiError.Body) == "" {
		return true
	}
	message := err.Error()
	for _, pattern := range nonOverflowPatterns {
		if pattern.MatchString(message) {
			return false
		}
	}
	for _, pattern := range overflowPatterns {
		if pattern.MatchString(message) {
			return true
		}
	}
	return false
}

type OverflowResponse struct {
	Overflow bool
	Retry    bool
}

func DetectContextOverflowResponse(response ChatResponse, contextWindow int) OverflowResponse {
	if contextWindow <= 0 {
		return OverflowResponse{}
	}
	inputTokens := response.InputTokens + response.CachedInputTokens
	finishReason := strings.ToLower(strings.TrimSpace(response.FinishReason))
	if finishReason == "stop" && inputTokens > contextWindow {
		return OverflowResponse{Overflow: true}
	}
	if finishReason == "length" && response.OutputTokens == 0 && float64(inputTokens) >= float64(contextWindow)*0.99 {
		return OverflowResponse{Overflow: true, Retry: true}
	}
	return OverflowResponse{}
}
