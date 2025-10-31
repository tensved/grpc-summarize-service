package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"grpc-summarize-service/config"
	pb "grpc-summarize-service/proto"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
)

// User-friendly error messages
const (
	ErrMsgTokenLimit   = "Too many messages to process. Please reduce the number of messages."
	ErrMsgAIService    = "AI service error. Please try again later."
	ErrMsgInvalidInput = "Invalid input data."
	ErrMsgInternal     = "Internal server error."
	ErrMsgTimeout      = "Request timeout exceeded."
	ErrMsgNetwork      = "Network error. Please check your connection."
)

// SummarizerServer implements gRPC server for summarization
type SummarizerServer struct {
	pb.UnimplementedSummarizerServiceServer
	httpClient *http.Client
	logger     zerolog.Logger
}

// NewSummarizerServer creates a new server instance
func NewSummarizerServer() *SummarizerServer {
	return &SummarizerServer{
		httpClient: &http.Client{
			Timeout: 2 * time.Minute, // Increased timeout for large requests
		},
		logger: initLogger(),
	}
}

// Summarize processes summarization requests with automatic retry
func (s *SummarizerServer) Summarize(ctx context.Context, req *pb.SummarizeRequest) (*pb.SummarizeResponse, error) {
	s.logger.Debug().
		Int("message_count", len(req.Messages)).
		Str("ai_model", config.Global.AIModel).
		Str("ai_service_url", config.Global.AIServiceURL).
		Msg("Starting summarization request")

	if len(req.Messages) == 0 {
		s.logger.Error().
			Msg("Empty message list provided")
		return &pb.SummarizeResponse{
			Summary:      "",
			MessageCount: 0,
			Status:       "error",
			Metadata:     ErrMsgInvalidInput,
		}, nil
	}

	// Create base AI request structure
	aiRequest := map[string]any{
		"model": config.Global.AIModel,
		"messages": []map[string]string{
			{
				"role":    "system",
				"content": config.Global.AISystemPrompt,
			},
			{
				"role":    "user",
				"content": "", // Will be filled in each attempt
			},
		},
	}

	maxRetries := 5
	currentModel := config.Global.AIModel
	backupModelUsed := false

	for attempt := range maxRetries {
		// Build chat content from current messages
		var chatContentBuilder strings.Builder
		for _, msg := range req.Messages {
			chatContentBuilder.WriteString(msg.Author)
			chatContentBuilder.WriteString(": ")
			chatContentBuilder.WriteString(msg.Content)
			chatContentBuilder.WriteString("\n")
		}
		chatContent := chatContentBuilder.String()

		aiRequest["messages"].([]map[string]string)[1]["content"] = chatContent
		aiRequest["model"] = currentModel // Use current model (primary or backup)

		s.logger.Debug().
			Int("attempt", attempt+1).
			Int("messages_count", len(req.Messages)).
			Int("content_length", len(chatContent)).
			Str("ai_model", currentModel).
			Msg("Attempting AI service call")

		summary, err := s.callAIService(ctx, aiRequest)
		if err != nil {
			if isTokenLimitError(err) {
				s.logger.Warn().
					Err(err).
					Int("attempt", attempt+1).
					Int("current_messages", len(req.Messages)).
					Int("content_length", len(chatContent)).
					Str("ai_model", currentModel).
					Msg("Token limit exceeded, reducing message count")

				if reduceErr := s.reduceMessageCount(req, chatContent); reduceErr != nil {
					s.logger.Error().
						Err(reduceErr).
						Int("final_messages", len(req.Messages)).
						Int("content_length", len(chatContent)).
						Str("ai_model", currentModel).
						Msg("Cannot reduce messages further, giving up")

					return &pb.SummarizeResponse{
						Summary:      "",
						MessageCount: int32(len(req.Messages)),
						Status:       "error",
						Metadata:     ErrMsgTokenLimit,
					}, nil
				}

				continue // Retry with reduced message count
			}

			// Check if it's a 503 error (service unavailable)
			if isServiceUnavailableError(err) {
				// Try switching to backup model on first 503 (if backup model exists and not already used)
				if !backupModelUsed && config.Global.AIBackupModel != "" && currentModel == config.Global.AIModel {
					s.logger.Warn().
						Err(err).
						Int("attempt", attempt+1).
						Int("messages_count", len(req.Messages)).
						Str("ai_service_url", config.Global.AIServiceURL).
						Str("primary_model", config.Global.AIModel).
						Str("backup_model", config.Global.AIBackupModel).
						Msg("Primary model returned 503, switching to backup model")

					// Switch to backup model
					currentModel = config.Global.AIBackupModel
					backupModelUsed = true
					continue // Retry immediately with backup model
				}

				// If backup model already used or no backup model - retry with delay
				s.logger.Warn().
					Err(err).
					Int("attempt", attempt+1).
					Int("messages_count", len(req.Messages)).
					Str("ai_service_url", config.Global.AIServiceURL).
					Str("ai_model", currentModel).
					Msg("AI service temporarily unavailable, retrying with delay")

				// Wait before retry (exponential backoff)
				delay := time.Duration(attempt+1) * 2 * time.Second
				time.Sleep(delay)
				continue // Retry the same request
			}

			s.logger.Error().
				Err(err).
				Int("attempt", attempt+1).
				Int("messages_count", len(req.Messages)).
				Str("ai_service_url", config.Global.AIServiceURL).
				Str("ai_model", currentModel).
				Msg("AI service error (not token limit or 503)")

			return &pb.SummarizeResponse{
				Summary:      "",
				MessageCount: int32(len(req.Messages)),
				Status:       "error",
				Metadata:     ErrMsgAIService,
			}, nil
		}

		s.logger.Debug().
			Int("attempt", attempt+1).
			Int("messages_processed", len(req.Messages)).
			Int("summary_length", len(summary)).
			Msg("Summarization completed successfully")

		// Second AI call to translate to English
		s.logger.Debug().Msg("Making second AI call to translate to English")

		translateRequest := map[string]any{
			"model": config.Global.AIModel,
			"messages": []map[string]string{
				{
					"role":    "system",
					"content": "You are a translator. Translate the following text to English. Use plain text format without any special characters like **, *, _, #. Only return the translated text, nothing else.",
				},
				{
					"role":    "user",
					"content": summary,
				},
			},
		}

		translatedSummary, err := s.callAIService(ctx, translateRequest)
		if err != nil {
			s.logger.Error().
				Err(err).
				Int("original_summary_length", len(summary)).
				Msg("Failed to translate summary to English")
			// Return original summary if translation fails
			translatedSummary = summary
		}
		s.logger.Info().
			Int("original_summary_length", len(summary)).
			Int("translated_summary_length", len(translatedSummary)).
			Msg("Successfully translated summary to English")

		return &pb.SummarizeResponse{
			Summary:      translatedSummary,
			MessageCount: int32(len(req.Messages)),
			Status:       "success",
			Metadata:     fmt.Sprintf("Processed %d messages (attempt %d)", len(req.Messages), attempt+1),
		}, nil
	}

	// If all attempts are exhausted
	s.logger.Error().
		Int("max_retries", maxRetries).
		Int("final_messages", len(req.Messages)).
		Str("ai_service_url", config.Global.AIServiceURL).
		Str("ai_model", config.Global.AIModel).
		Msg("All retry attempts exhausted")

	return &pb.SummarizeResponse{
		Summary:      "",
		MessageCount: int32(len(req.Messages)),
		Status:       "error",
		Metadata:     ErrMsgAIService,
	}, nil
}

// callAIService calls AI service to get summarization
func (s *SummarizerServer) callAIService(ctx context.Context, request map[string]any) (string, error) {
	s.logger.Debug().
		Str("ai_service_url", config.Global.AIServiceURL).
		Str("ai_model", config.Global.AIModel).
		Msg("Calling AI service")

	jsonData, err := json.Marshal(request)
	if err != nil {
		s.logger.Error().
			Err(err).
			Str("ai_service_url", config.Global.AIServiceURL).
			Str("ai_model", config.Global.AIModel).
			Msg("Failed to marshal request to JSON")
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", config.Global.AIServiceURL, bytes.NewBuffer(jsonData))
	if err != nil {
		s.logger.Error().
			Err(err).
			Str("ai_service_url", config.Global.AIServiceURL).
			Str("method", "POST").
			Int("request_size", len(jsonData)).
			Msg("Failed to create HTTP request")
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+config.Global.AIToken)

	s.logger.Debug().
		Str("ai_service_url", config.Global.AIServiceURL).
		Int("request_size", len(jsonData)).
		Msg("Sending HTTP request to AI service")

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		s.logger.Error().
			Err(err).
			Str("ai_service_url", config.Global.AIServiceURL).
			Int("request_size", len(jsonData)).
			Msg("Failed to call AI service - network error")
		return "", fmt.Errorf("failed to call AI service: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		s.logger.Error().
			Err(err).
			Str("ai_service_url", config.Global.AIServiceURL).
			Int("status_code", resp.StatusCode).
			Msg("Failed to read response body")
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	s.logger.Debug().
		Str("ai_service_url", config.Global.AIServiceURL).
		Int("status_code", resp.StatusCode).
		Int("response_size", len(body)).
		Msg("Received response from AI service")

	if resp.StatusCode != http.StatusOK {
		s.logger.Error().
			Str("ai_service_url", config.Global.AIServiceURL).
			Int("status_code", resp.StatusCode).
			Str("response_body", string(body)).
			Int("request_size", len(jsonData)).
			Msg("AI service returned non-OK status")
		return "", fmt.Errorf("AI service returned status %d", resp.StatusCode)
	}

	var aiResponse map[string]any
	if err := json.Unmarshal(body, &aiResponse); err != nil {
		s.logger.Error().
			Err(err).
			Str("ai_service_url", config.Global.AIServiceURL).
			Int("response_size", len(body)).
			Str("response_body", string(body)).
			Msg("Failed to unmarshal AI service response")
		return "", fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if choices, ok := aiResponse["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if message, ok := choice["message"].(map[string]any); ok {
				if content, ok := message["content"].(string); ok {
					s.logger.Debug().
						Str("ai_service_url", config.Global.AIServiceURL).
						Int("content_length", len(content)).
						Msg("Successfully extracted content from AI response")
					return content, nil
				}
			}
		}
	}

	s.logger.Error().
		Str("ai_service_url", config.Global.AIServiceURL).
		Int("response_size", len(body)).
		Str("response_body", string(body)).
		Msg("Unexpected response format from AI service")
	return "", fmt.Errorf("unexpected response format from AI service")
}

// reduceMessageCount reduces message count directly in req.Messages to fit token limit
func (s *SummarizerServer) reduceMessageCount(req *pb.SummarizeRequest, chatContent string) error {
	currentRunes := len([]rune(chatContent))
	originalMessageCount := len(req.Messages)

	s.logger.Debug().
		Int("original_messages", originalMessageCount).
		Int("current_runes", currentRunes).
		Msg("Starting message reduction")

	// Determine target rune count
	var targetRunes int
	if currentRunes > 35000 {
		targetRunes = 35000
	} else {
		targetRunes = int(float64(currentRunes) * 0.8)
	}

	excessRunes := currentRunes - targetRunes

	s.logger.Debug().
		Int("target_runes", targetRunes).
		Int("excess_runes", excessRunes).
		Float64("reduction_ratio", float64(targetRunes)/float64(currentRunes)).
		Msg("Calculated reduction parameters")

	// Go through messages in chronological order
	removedRunes := 0
	keepFromIndex := 0

	for i, msg := range req.Messages {
		messageRunes := len([]rune(msg.Content))

		removedRunes += messageRunes

		if removedRunes >= excessRunes {
			keepFromIndex = i + 1
			break
		}
	}

	if keepFromIndex >= len(req.Messages) {
		s.logger.Error().
			Int("original_messages", originalMessageCount).
			Int("current_runes", currentRunes).
			Int("target_runes", targetRunes).
			Int("excess_runes", excessRunes).
			Msg("Cannot reduce messages: all messages would be removed")
		return fmt.Errorf("cannot reduce messages: all messages would be removed")
	}

	req.Messages = req.Messages[keepFromIndex:]

	s.logger.Info().
		Int("original_messages", originalMessageCount).
		Int("reduced_messages", len(req.Messages)).
		Int("removed_messages", originalMessageCount-len(req.Messages)).
		Int("original_runes", currentRunes).
		Int("removed_runes", removedRunes).
		Int("keep_from_index", keepFromIndex).
		Msg("Successfully reduced message count")

	return nil
}

// isTokenLimitError checks if error is related to token limit exceeded
func isTokenLimitError(err error) bool {
	errorStr := err.Error()

	tokenLimitIndicators := []string{
		"maximum context length",
		"reduce the length",
	}

	for _, indicator := range tokenLimitIndicators {
		if strings.Contains(strings.ToLower(errorStr), strings.ToLower(indicator)) {
			return true
		}
	}

	return false
}

// isServiceUnavailableError checks if error is 503 Service Unavailable
func isServiceUnavailableError(err error) bool {
	errorStr := err.Error()
	return strings.Contains(errorStr, "503") || strings.Contains(errorStr, "Service unavailable")
}

// RegisterServer registers server in gRPC
func (s *SummarizerServer) RegisterServer(grpcServer *grpc.Server) {
	pb.RegisterSummarizerServiceServer(grpcServer, s)
}

// initLogger initializes logger with level from config
func initLogger() zerolog.Logger {
	level, err := zerolog.ParseLevel(config.Global.LogLevel)
	if err != nil {
		level = zerolog.DebugLevel
	}

	return zerolog.New(zerolog.NewConsoleWriter()).
		With().
		Timestamp().
		Logger().
		Level(level)
}
