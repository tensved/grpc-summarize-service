package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"grpc-summarize-service/config"
	pb "grpc-summarize-service/proto"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// User-friendly error messages for Matrix operations
const (
	ErrMsgMatrixAuthFailed     = "Matrix authentication failed. Please check your credentials."
	ErrMsgMatrixRoomNotFound   = "Room not found or access denied."
	ErrMsgMatrixInvalidRequest = "Invalid request parameters."
	ErrMsgMatrixInternalError  = "Internal Matrix service error."
	ErrMsgMatrixNetworkError   = "Network error connecting to Matrix server."
)

// SummarizeMatrixRoom processes Matrix room summarization requests
func (s *SummarizerServer) SummarizeMatrixRoom(
	ctx context.Context,
	req *pb.SummarizeMatrixRoomRequest,
) (*pb.SummarizeMatrixRoomResponse, error) {

	s.logger.Debug().
		Str("homeserver", req.MatrixHomeserverUrl).
		Str("username", req.MatrixUsername).
		Str("room_id", req.RoomId).
		Str("room_alias", req.RoomAlias).
		Int32("message_count", req.MessageCount).
		Msg("Processing Matrix room summarization request")

	// 1. Input data validation
	if err := s.validateMatrixRequest(req); err != nil {
		s.logger.Error().Err(err).Msg("Matrix request validation failed")
		return &pb.SummarizeMatrixRoomResponse{
			Summary: ErrMsgMatrixInvalidRequest,
		}, nil
	}

	// 2. Matrix client creation and authentication
	matrixClient, err := s.createMatrixClientWithUserCredentials(req)
	if err != nil {
		s.logger.Error().Err(err).Msg("Failed to create Matrix client")
		return &pb.SummarizeMatrixRoomResponse{
			Summary: ErrMsgMatrixAuthFailed,
		}, nil
	}

	// 3. Access rights verification
	userID, err := s.validateUserAuthentication(matrixClient)
	if err != nil {
		s.logger.Error().Err(err).Msg("User authentication validation failed")
		return &pb.SummarizeMatrixRoomResponse{
			Summary: ErrMsgMatrixAuthFailed,
		}, nil
	}

	// 4. Resolve room ID if needed (if room_alias was provided, resolve it to room_id)
	if req.RoomId == "" && req.RoomAlias != "" {
		req.RoomId, err = s.resolveRoomAlias(matrixClient, req.RoomAlias)
		if err != nil {
			s.logger.Error().Err(err).Str("room_alias", req.RoomAlias).Msg("Failed to resolve room alias")
			return &pb.SummarizeMatrixRoomResponse{
				Summary: ErrMsgMatrixRoomNotFound,
			}, nil
		}
	}

	// 5. Fetch messages from room
	messages, err := s.fetchMessagesFromRoom(matrixClient, id.RoomID(req.RoomId), req)
	if err != nil {
		s.logger.Error().
			Err(err).
			Str("room_id", req.RoomId).
			Str("user_id", userID.String()).
			Msg("Failed to fetch messages from Matrix room")
		return &pb.SummarizeMatrixRoomResponse{
			Summary: ErrMsgMatrixRoomNotFound,
		}, nil
	}

	// 6. Perform summarization
	summary, err := s.callAIServiceForMatrix(ctx, messages)
	if err != nil {
		s.logger.Error().Err(err).Msg("Failed to perform Matrix room summarization")
		return &pb.SummarizeMatrixRoomResponse{
			Summary: err.Error(),
		}, nil
	}

	s.logger.Info().
		Str("user_id", userID.String()).
		Str("room_id", req.RoomId).
		Int("message_count", len(messages)).
		Int("summary_length", len(summary)).
		Msg("Matrix room summarization completed successfully")

	// Return only summary for Matrix room requests
	return &pb.SummarizeMatrixRoomResponse{
		Summary: summary,
	}, nil
}

// validateMatrixRequest validates request input data
func (s *SummarizerServer) validateMatrixRequest(req *pb.SummarizeMatrixRoomRequest) error {
	s.logger.Debug().
		Str("username", req.MatrixUsername).
		Str("homeserver_url", req.MatrixHomeserverUrl).
		Str("servername", req.MatrixServername).
		Str("room_id", req.RoomId).
		Str("room_alias", req.RoomAlias).
		Int32("message_count", req.MessageCount).
		Msg("Validating Matrix request parameters")

	// Required fields validation
	if req.MatrixUsername == "" {
		return fmt.Errorf("matrix_username is required")
	}
	if req.MatrixPassword == "" {
		return fmt.Errorf("matrix_password is required")
	}

	// Server parameters validation
	if req.MatrixHomeserverUrl == "" && req.MatrixServername == "" {
		return fmt.Errorf("either matrix_homeserver_url or matrix_servername is required")
	}

	// Normalize homeserver_url and servername - ensure both are set
	if req.MatrixHomeserverUrl != "" && req.MatrixServername == "" {
		// Extract servername from homeserver URL
		servername := req.MatrixHomeserverUrl
		if rest, ok := strings.CutPrefix(servername, "https://"); ok {
			servername = rest
		} else if rest, ok := strings.CutPrefix(servername, "http://"); ok {
			servername = rest
		}
		req.MatrixServername = servername
		s.logger.Debug().
			Str("extracted_servername", servername).
			Str("from_homeserver_url", req.MatrixHomeserverUrl).
			Msg("Extracted servername from homeserver URL")
	} else if req.MatrixServername != "" && req.MatrixHomeserverUrl == "" {
		// Generate homeserver URL from servername
		req.MatrixHomeserverUrl = fmt.Sprintf("https://%s", req.MatrixServername)
		s.logger.Debug().
			Str("generated_homeserver_url", req.MatrixHomeserverUrl).
			Str("from_servername", req.MatrixServername).
			Msg("Generated homeserver URL from servername")
	}

	// Room parameters validation
	if req.RoomId == "" && req.RoomAlias == "" {
		return fmt.Errorf("either room_id or room_alias is required")
	}

	// Auto-complete RoomId with servername if needed
	// req.MatrixServername is guaranteed to be set after normalization above
	if req.RoomId != "" && !strings.Contains(req.RoomId, ":") {
		req.RoomId = req.RoomId + ":" + req.MatrixServername
		s.logger.Info().
			Str("completed_room_id", req.RoomId).
			Msg("Auto-completed RoomId with servername")
	}

	// Auto-complete RoomAlias with servername if needed
	// req.MatrixServername is guaranteed to be set after normalization above
	if req.RoomAlias != "" && !strings.Contains(req.RoomAlias, ":") {
		req.RoomAlias = req.RoomAlias + ":" + req.MatrixServername
		s.logger.Info().
			Str("completed_room_alias", req.RoomAlias).
			Msg("Auto-completed RoomAlias with servername")
	}

	// Message count validation and adjustment
	if req.MessageCount <= 0 {
		s.logger.Warn().
			Int32("invalid_count", req.MessageCount).
			Int32("default_count", config.Global.MaxMessages).
			Msg("Invalid message_count, setting to default")
		req.MessageCount = config.Global.MaxMessages
	}
	if req.MessageCount > config.Global.MaxMessages {
		s.logger.Warn().
			Int32("requested_count", req.MessageCount).
			Int32("max_allowed", config.Global.MaxMessages).
			Msg("message_count exceeds limit, setting to maximum")
		req.MessageCount = config.Global.MaxMessages
	}

	s.logger.Debug().Msg("Matrix request validation completed successfully")
	return nil
}

// createMatrixClientWithUserCredentials creates Matrix client with user credentials
func (s *SummarizerServer) createMatrixClientWithUserCredentials(
	req *pb.SummarizeMatrixRoomRequest,
) (*mautrix.Client, error) {
	// Both homeserver_url and servername are normalized in validateMatrixRequest
	homeserverURL := req.MatrixHomeserverUrl

	s.logger.Debug().
		Str("homeserver", homeserverURL).
		Str("username", req.MatrixUsername).
		Msg("Creating Matrix client")

	// Create new Matrix client
	client, err := mautrix.NewClient(homeserverURL, "", "")
	if err != nil {
		return nil, fmt.Errorf("failed to create matrix client: %w", err)
	}

	// Authenticate with user credentials
	resp, err := client.Login(context.Background(), &mautrix.ReqLogin{
		Type:             "m.login.password",
		Identifier:       mautrix.UserIdentifier{Type: "m.id.user", User: req.MatrixUsername},
		Password:         req.MatrixPassword,
		StoreCredentials: true,
	})
	if err != nil {
		s.logger.Error().
			Err(err).
			Str("username", req.MatrixUsername).
			Str("homeserver", homeserverURL).
			Msg("Matrix authentication failed")
		return nil, fmt.Errorf("matrix authentication failed: %w", err)
	}

	s.logger.Info().
		Str("user_id", resp.UserID.String()).
		Str("username", req.MatrixUsername).
		Str("homeserver", homeserverURL).
		Msg("Matrix user authenticated successfully")

	return client, nil
}

// validateUserAuthentication validates user authentication
func (s *SummarizerServer) validateUserAuthentication(
	client *mautrix.Client,
) (id.UserID, error) {

	s.logger.Debug().Msg("Validating user authentication")

	// Get current user information
	whoami, err := client.Whoami(context.Background())
	if err != nil {
		s.logger.Error().Err(err).Msg("Failed to get user info from Matrix")
		return "", fmt.Errorf("failed to get user info: %w", err)
	}

	userID := whoami.UserID
	s.logger.Debug().
		Str("user_id", userID.String()).
		Msg("User authentication validated successfully")

	return userID, nil
}

// resolveRoomAlias resolves room alias to room_id
func (s *SummarizerServer) resolveRoomAlias(
	client *mautrix.Client,
	roomAlias string,
) (string, error) {

	s.logger.Debug().
		Str("room_alias", roomAlias).
		Msg("Resolving Matrix room alias")

	// Get room information by alias
	roomInfo, err := client.ResolveAlias(context.Background(), id.RoomAlias(roomAlias))
	if err != nil {
		s.logger.Error().
			Err(err).
			Str("room_alias", roomAlias).
			Msg("Failed to resolve Matrix room alias")
		return "", fmt.Errorf("failed to resolve room alias %s: %w", roomAlias, err)
	}

	s.logger.Debug().
		Str("room_alias", roomAlias).
		Str("room_id", roomInfo.RoomID.String()).
		Msg("Matrix room alias resolved successfully")

	return roomInfo.RoomID.String(), nil
}

// fetchMessagesFromRoom gets messages from Matrix room
func (s *SummarizerServer) fetchMessagesFromRoom(
	client *mautrix.Client,
	roomID id.RoomID,
	req *pb.SummarizeMatrixRoomRequest,
) ([]*pb.Message, error) {

	s.logger.Debug().
		Str("room_id", roomID.String()).
		Int32("requested_count", req.MessageCount).
		Msg("Fetching messages from Matrix room")

	// Create filter to get only m.room.message events
	filter := &mautrix.FilterPart{
		Types: []event.Type{event.EventMessage},
	}

	// Get message history with filter, starting from the end ('b' - backwards)
	messages, err := client.Messages(context.Background(), roomID, "", "", 'b', filter, int(req.MessageCount))
	if err != nil {
		s.logger.Error().
			Err(err).
			Str("room_id", roomID.String()).
			Msg("Failed to get messages from Matrix room")
		return nil, fmt.Errorf("failed to get messages: %w", err)
	}

	s.logger.Debug().
		Str("room_id", roomID.String()).
		Int("raw_events", len(messages.Chunk)).
		Msg("Retrieved raw events from Matrix API")

	// Convert to protobuf format
	protoMessages := make([]*pb.Message, 0, len(messages.Chunk))
	skippedCount := 0

	// Messages come in reverse order, need to reverse for chronological order
	for i := len(messages.Chunk) - 1; i >= 0; i-- {
		event := messages.Chunk[i]

		// Types filter guarantees only m.room.message events
		// Additionally check msgtype for text messages
		if msgType, ok := event.Content.Raw["msgtype"].(string); ok && msgType == "m.text" {
			sender := event.Sender.String()
			body, _ := event.Content.Raw["body"].(string)

			s.logger.Debug().
				Str("sender", sender).
				Str("content_preview", truncateString(body, 50)).
				Msg("Processing text message")

			// Extract real time from event
			timestamp := event.Timestamp
			if timestamp == 0 {
				timestamp = time.Now().Unix()
				s.logger.Warn().Msg("Message had no timestamp, using current time")
			}

			// Create protobuf message
			protoMsg := &pb.Message{
				Author:      sender,
				Content:     body,
				Timestamp:   timestamp,
				MessageType: "text",
			}

			protoMessages = append(protoMessages, protoMsg)
		} else {
			skippedCount++
			if msgType, ok := event.Content.Raw["msgtype"].(string); ok {
				s.logger.Debug().
					Str("msgtype", msgType).
					Msg("Skipping non-text message")
			} else {
				s.logger.Debug().Msg("Skipping message without msgtype")
			}
		}
	}

	s.logger.Info().
		Str("room_id", roomID.String()).
		Int("total_events", len(messages.Chunk)).
		Int("text_messages", len(protoMessages)).
		Int("skipped_messages", skippedCount).
		Msg("Matrix messages processing completed successfully")

	return protoMessages, nil
}

// truncateString truncates string to specified length
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// callAIServiceForMatrix calls AI service to get Matrix messages summarization
func (s *SummarizerServer) callAIServiceForMatrix(ctx context.Context, messages []*pb.Message) (string, error) {
	s.logger.Debug().
		Int("message_count", len(messages)).
		Msg("Calling AI service for Matrix messages summarization")

	// Create summarization request
	summarizeReq := &pb.SummarizeRequest{
		Messages: messages,
	}

	// Call summarization method
	response, err := s.Summarize(ctx, summarizeReq)
	if err != nil {
		s.logger.Error().
			Err(err).
			Int("message_count", len(messages)).
			Msg("AI summarization failed for Matrix messages")
		return "", err
	}

	// Check if Summarize returned an error in status field
	if response.Status == "error" {
		s.logger.Error().
			Str("status", response.Status).
			Str("metadata", response.Metadata).
			Int("message_count", len(messages)).
			Msg("AI summarization failed - error in status field")
		return "", fmt.Errorf("summarization failed: %s", response.Metadata)
	}

	s.logger.Info().
		Int("message_count", len(messages)).
		Int("summary_length", len(response.Summary)).
		Msg("AI summarization completed successfully for Matrix messages")

	return response.Summary, nil
}
