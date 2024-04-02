package openai

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/gptscript-ai/gptscript/pkg/cache"
	"github.com/gptscript-ai/gptscript/pkg/hash"
	"github.com/gptscript-ai/gptscript/pkg/system"
	"github.com/gptscript-ai/gptscript/pkg/types"
	"github.com/sashabaranov/go-openai"
)

const (
	DefaultModel = openai.GPT4TurboPreview
)

var (
	key          = os.Getenv("OPENAI_API_KEY")
	url          = os.Getenv("OPENAI_URL")
	azureModel   = os.Getenv("OPENAI_AZURE_DEPLOYMENT")
	completionID int64
)

type Client struct {
	defaultModel string
	c            *openai.Client
	cache        *cache.Client
	invalidAuth  bool
	cacheKeyBase string
	setSeed      bool
	user         string
}

type Options struct {
	BaseURL      string         `usage:"OpenAI base URL" name:"openai-base-url" env:"OPENAI_BASE_URL"`
	APIKey       string         `usage:"OpenAI API KEY" name:"openai-api-key" env:"OPENAI_API_KEY"`
	APIVersion   string         `usage:"OpenAI API Version (for Azure)" name:"openai-api-version" env:"OPENAI_API_VERSION"`
	APIType      openai.APIType `usage:"OpenAI API Type (valid: OPEN_AI, AZURE, AZURE_AD)" name:"openai-api-type" env:"OPENAI_API_TYPE"`
	OrgID        string         `usage:"OpenAI organization ID" name:"openai-org-id" env:"OPENAI_ORG_ID"`
	User         string         `usage:"OpenAI user" name:"openai-user" env:"OPENAI_USER"`
	DefaultModel string         `usage:"Default LLM model to use" default:"gpt-4-turbo-preview"`
	SetSeed      bool           `usage:"-"`
	CacheKey     string         `usage:"-"`
	Cache        *cache.Client
}

func complete(opts ...Options) (result Options, err error) {
	for _, opt := range opts {
		result.User = types.FirstSet(opt.User, result.User)
		result.BaseURL = types.FirstSet(opt.BaseURL, result.BaseURL)
		result.APIKey = types.FirstSet(opt.APIKey, result.APIKey)
		result.OrgID = types.FirstSet(opt.OrgID, result.OrgID)
		result.Cache = types.FirstSet(opt.Cache, result.Cache)
		result.APIVersion = types.FirstSet(opt.APIVersion, result.APIVersion)
		result.APIType = types.FirstSet(opt.APIType, result.APIType)
		result.DefaultModel = types.FirstSet(opt.DefaultModel, result.DefaultModel)
		result.SetSeed = types.FirstSet(opt.SetSeed, result.SetSeed)
		result.CacheKey = types.FirstSet(opt.CacheKey, result.CacheKey)
	}

	if result.Cache == nil {
		result.Cache, err = cache.New(cache.Options{
			Cache: new(bool),
		})
	}

	if result.BaseURL == "" && url != "" {
		result.BaseURL = url
	}

	if result.APIKey == "" && key != "" {
		result.APIKey = key
	}

	return result, err
}

func GetAzureMapperFunction(defaultModel, azureModel string) func(string) string {
	if azureModel == "" {
		return func(model string) string {
			return model
		}
	}
	return func(model string) string {
		return map[string]string{
			defaultModel: azureModel,
		}[model]
	}
}

func NewClient(opts ...Options) (*Client, error) {
	opt, err := complete(opts...)
	if err != nil {
		return nil, err
	}

	cfg := openai.DefaultConfig(opt.APIKey)
	if strings.Contains(string(opt.APIType), "AZURE") {
		cfg = openai.DefaultAzureConfig(key, url)
		cfg.AzureModelMapperFunc = GetAzureMapperFunction(opt.DefaultModel, azureModel)
	}

	cfg.BaseURL = types.FirstSet(opt.BaseURL, cfg.BaseURL)
	cfg.OrgID = types.FirstSet(opt.OrgID, cfg.OrgID)
	cfg.APIVersion = types.FirstSet(opt.APIVersion, cfg.APIVersion)
	cfg.APIType = types.FirstSet(opt.APIType, cfg.APIType)

	cacheKeyBase := opt.CacheKey
	if cacheKeyBase == "" {
		cacheKeyBase = hash.ID(opt.APIKey, opt.BaseURL)
	}

	return &Client{
		c:            openai.NewClientWithConfig(cfg),
		cache:        opt.Cache,
		defaultModel: opt.DefaultModel,
		cacheKeyBase: cacheKeyBase,
		invalidAuth:  opt.APIKey == "" && opt.BaseURL == "",
		setSeed:      opt.SetSeed,
		user:         opt.User,
	}, nil
}

func (c *Client) ValidAuth() error {
	if c.invalidAuth {
		return fmt.Errorf("OPENAI_API_KEY is not set. Please set the OPENAI_API_KEY environment variable")
	}
	return nil
}

func (c *Client) Supports(ctx context.Context, modelName string) (bool, error) {
	models, err := c.ListModels(ctx)
	if err != nil {
		return false, err
	}
	return slices.Contains(models, modelName), nil
}

func (c *Client) ListModels(ctx context.Context, providers ...string) (result []string, _ error) {
	// Only serve if providers is empty or "" is in the list
	if len(providers) != 0 && !slices.Contains(providers, "") {
		return nil, nil
	}

	if err := c.ValidAuth(); err != nil {
		return nil, err
	}

	models, err := c.c.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	for _, model := range models.Models {
		result = append(result, model.ID)
	}
	sort.Strings(result)
	return result, nil
}

func (c *Client) cacheKey(request openai.ChatCompletionRequest) string {
	return hash.Encode(map[string]any{
		"base":    c.cacheKeyBase,
		"request": request,
	})
}

func (c *Client) seed(request openai.ChatCompletionRequest) int {
	newRequest := request
	newRequest.Messages = nil

	for _, msg := range request.Messages {
		newMsg := msg
		newMsg.ToolCalls = nil
		newMsg.ToolCallID = ""

		for _, tool := range msg.ToolCalls {
			tool.ID = ""
			newMsg.ToolCalls = append(newMsg.ToolCalls, tool)
		}

		newRequest.Messages = append(newRequest.Messages, newMsg)
	}
	return hash.Seed(newRequest)
}

func (c *Client) fromCache(ctx context.Context, messageRequest types.CompletionRequest, request openai.ChatCompletionRequest) (result []openai.ChatCompletionStreamResponse, _ bool, _ error) {
	if cache.IsNoCache(ctx) {
		return nil, false, nil
	}
	if messageRequest.Cache != nil && !*messageRequest.Cache {
		return nil, false, nil
	}

	cache, found, err := c.cache.Get(c.cacheKey(request))
	if err != nil {
		return nil, false, err
	} else if !found {
		return nil, false, nil
	}

	gz, err := gzip.NewReader(bytes.NewReader(cache))
	if err != nil {
		return nil, false, err
	}
	return result, true, json.NewDecoder(gz).Decode(&result)
}

func toToolCall(call types.CompletionToolCall) openai.ToolCall {
	return openai.ToolCall{
		Index: call.Index,
		ID:    call.ID,
		Type:  openai.ToolTypeFunction,
		Function: openai.FunctionCall{
			Name:      call.Function.Name,
			Arguments: call.Function.Arguments,
		},
	}
}

func toMessages(request types.CompletionRequest) (result []openai.ChatCompletionMessage, err error) {
	var (
		systemPrompts []string
		msgs          []types.CompletionMessage
	)

	if request.InternalSystemPrompt == nil || *request.InternalSystemPrompt {
		systemPrompts = append(systemPrompts, system.InternalSystemPrompt)
	}

	for i, message := range request.Messages {
		if message.Role == types.CompletionMessageRoleTypeSystem {
			// Append if the next message is system or user, otherwise set as user message
			if i == len(request.Messages)-1 ||
				(request.Messages[i+1].Role != types.CompletionMessageRoleTypeSystem &&
					request.Messages[i+1].Role != types.CompletionMessageRoleTypeUser) {
				message.Role = types.CompletionMessageRoleTypeUser
			} else {
				systemPrompts = append(systemPrompts, message.Content[0].Text)
				continue
			}
		}
		msgs = append(msgs, message)
	}

	if len(systemPrompts) > 0 {
		msgs = slices.Insert(msgs, 0, types.CompletionMessage{
			Role:    types.CompletionMessageRoleTypeSystem,
			Content: types.Text(strings.Join(systemPrompts, "\n")),
		})
	}

	for _, message := range msgs {
		chatMessage := openai.ChatCompletionMessage{
			Role: string(message.Role),
		}

		if message.ToolCall != nil {
			chatMessage.ToolCallID = message.ToolCall.ID
			// This field is not documented but specifically Azure thinks it should be set
			chatMessage.Name = message.ToolCall.Function.Name
		}

		for _, content := range message.Content {
			if content.ToolCall != nil {
				chatMessage.ToolCalls = append(chatMessage.ToolCalls, toToolCall(*content.ToolCall))
			}
			if content.Text != "" {
				chatMessage.MultiContent = append(chatMessage.MultiContent, openai.ChatMessagePart{
					Type: openai.ChatMessagePartTypeText,
					Text: content.Text,
				})
			}
		}

		if len(chatMessage.MultiContent) == 1 && chatMessage.MultiContent[0].Type == openai.ChatMessagePartTypeText {
			if chatMessage.MultiContent[0].Text == "." || chatMessage.MultiContent[0].Text == "{}" {
				continue
			}
			chatMessage.Content = chatMessage.MultiContent[0].Text
			chatMessage.MultiContent = nil

			if prompt, ok := system.IsDefaultPrompt(chatMessage.Content); ok {
				chatMessage.Content = prompt
			}
		}

		result = append(result, chatMessage)
	}

	return
}

func (c *Client) Call(ctx context.Context, messageRequest types.CompletionRequest, status chan<- types.CompletionStatus) (*types.CompletionMessage, error) {
	if err := c.ValidAuth(); err != nil {
		return nil, err
	}

	if messageRequest.Model == "" {
		messageRequest.Model = c.defaultModel
	}
	msgs, err := toMessages(messageRequest)
	if err != nil {
		return nil, err
	}

	if len(msgs) == 0 {
		return nil, fmt.Errorf("invalid request, no messages to send to OpenAI")
	}

	request := openai.ChatCompletionRequest{
		Model:     messageRequest.Model,
		Messages:  msgs,
		MaxTokens: messageRequest.MaxTokens,
		User:      c.user,
	}

	if messageRequest.Temperature == nil {
		request.Temperature = new(float32)
	} else {
		request.Temperature = messageRequest.Temperature
	}

	if messageRequest.JSONResponse {
		request.ResponseFormat = &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONObject,
		}
	}

	for _, tool := range messageRequest.Tools {
		params := tool.Function.Parameters

		request.Tools = append(request.Tools, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        tool.Function.Name,
				Description: tool.Function.Description,
				Parameters:  params,
			},
		})
	}

	id := fmt.Sprint(atomic.AddInt64(&completionID, 1))
	status <- types.CompletionStatus{
		CompletionID: id,
		Request:      request,
	}

	var cacheResponse bool
	if c.setSeed {
		request.Seed = ptr(c.seed(request))
	}
	response, ok, err := c.fromCache(ctx, messageRequest, request)
	if err != nil {
		return nil, err
	} else if !ok {
		response, err = c.call(ctx, request, id, status)
		if err != nil {
			return nil, err
		}
	} else {
		cacheResponse = true
	}

	result := types.CompletionMessage{}
	for _, response := range response {
		result = appendMessage(result, response)
	}

	for i, content := range result.Content {
		if content.ToolCall != nil && content.ToolCall.ID == "" {
			content.ToolCall.ID = "call_" + hash.ID(content.ToolCall.Function.Name, content.ToolCall.Function.Arguments)[:8]
			result.Content[i] = content
		}
	}

	status <- types.CompletionStatus{
		CompletionID: id,
		Chunks:       response,
		Response:     result,
		Cached:       cacheResponse,
	}

	return &result, nil
}

func appendMessage(msg types.CompletionMessage, response openai.ChatCompletionStreamResponse) types.CompletionMessage {
	if len(response.Choices) == 0 {
		return msg
	}

	delta := response.Choices[0].Delta
	msg.Role = types.CompletionMessageRoleType(override(string(msg.Role), delta.Role))

	for _, tool := range delta.ToolCalls {
		idx := 0
		if tool.Index != nil {
			idx = *tool.Index
		}
		for len(msg.Content)-1 < idx {
			msg.Content = append(msg.Content, types.ContentPart{
				ToolCall: &types.CompletionToolCall{
					Index: ptr(len(msg.Content)),
				},
			})
		}

		tc := msg.Content[idx]
		if tc.ToolCall == nil {
			tc.ToolCall = &types.CompletionToolCall{}
		}
		if tool.Index != nil {
			tc.ToolCall.Index = tool.Index
		}
		tc.ToolCall.ID = override(tc.ToolCall.ID, tool.ID)
		tc.ToolCall.Function.Name += tool.Function.Name
		tc.ToolCall.Function.Arguments += tool.Function.Arguments

		msg.Content[idx] = tc
	}

	if delta.Content != "" {
		found := false
		for i, content := range msg.Content {
			if content.ToolCall != nil {
				continue
			}
			msg.Content[i] = types.ContentPart{
				Text: msg.Content[i].Text + delta.Content,
			}
			found = true
			break
		}
		if !found {
			msg.Content = append(msg.Content, types.ContentPart{
				Text: delta.Content,
			})
		}
	}

	return msg
}

func override(left, right string) string {
	if right != "" {
		return right
	}
	return left
}

func (c *Client) store(ctx context.Context, key string, responses []openai.ChatCompletionStreamResponse) error {
	if cache.IsNoCache(ctx) {
		return nil
	}
	buf := &bytes.Buffer{}
	gz := gzip.NewWriter(buf)
	err := json.NewEncoder(gz).Encode(responses)
	if err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return c.cache.Store(key, buf.Bytes())
}

func (c *Client) call(ctx context.Context, request openai.ChatCompletionRequest, transactionID string, partial chan<- types.CompletionStatus) (responses []openai.ChatCompletionStreamResponse, _ error) {
	cacheKey := c.cacheKey(request)
	request.Stream = true

	partial <- types.CompletionStatus{
		CompletionID: transactionID,
		PartialResponse: &types.CompletionMessage{
			Role:    types.CompletionMessageRoleTypeAssistant,
			Content: types.Text("Waiting for model response..."),
		},
	}

	slog.Debug("calling openai", "message", request.Messages)
	stream, err := c.c.CreateChatCompletionStream(ctx, request)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	var partialMessage types.CompletionMessage
	for {
		response, err := stream.Recv()
		if err == io.EOF {
			return responses, c.store(ctx, cacheKey, responses)
		} else if err != nil {
			return nil, err
		}
		if len(response.Choices) > 0 {
			slog.Debug("stream", "content", response.Choices[0].Delta.Content)
		}
		if partial != nil {
			partialMessage = appendMessage(partialMessage, response)
			partial <- types.CompletionStatus{
				CompletionID:    transactionID,
				PartialResponse: &partialMessage,
			}
		}
		responses = append(responses, response)
	}
}

func ptr[T any](v T) *T {
	return &v
}
