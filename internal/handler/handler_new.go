package handler

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"fune/internal/config"
	"fune/internal/delivery"
	"fune/internal/dkim"
	"fune/internal/queue"
	"fune/internal/recovery"

	"github.com/emersion/go-message/mail"
	"go.uber.org/zap"
)

// GossipService interface for gossip protocol integration
type GossipService interface {
	BroadcastIdempotencyKey(key string) error
	CheckIdempotencyKey(key string) string
}

// QueueMessageHandler handles HTTP requests and enqueues messages
type QueueMessageHandler struct {
	queue          *queue.Queue
	deliveryConfig *config.OutboundConfig
	httpConfig     *config.InboundConfig
	circuitBreaker *delivery.CircuitBreaker
	gossip         GossipService
	logger         *zap.Logger
}

// NewQueueMessageHandler creates a new queue-based message handler
func NewQueueMessageHandler(q *queue.Queue, deliveryCfg *config.OutboundConfig, httpCfg *config.InboundConfig, circuitBreaker *delivery.CircuitBreaker, logger *zap.Logger) *QueueMessageHandler {
	return &QueueMessageHandler{
		queue:          q,
		deliveryConfig: deliveryCfg,
		httpConfig:     httpCfg,
		circuitBreaker: circuitBreaker,
		gossip:         nil, // Set via SetGossip() after initialization
		logger:         logger,
	}
}

// SetGossip sets the gossip service for distributed idempotency
func (h *QueueMessageHandler) SetGossip(gossip GossipService) {
	h.gossip = gossip
}

// ServeHTTP handles incoming message submission requests
func (h *QueueMessageHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer recovery.RecoverPanicWithCallback(h.logger, "HTTP handler", func(p interface{}) {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	})
	start := time.Now()

	// Log incoming request
	h.logger.Info("incoming request",
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path),
		zap.String("remote_addr", r.RemoteAddr),
		zap.String("content_type", r.Header.Get("Content-Type")))

	if r.Method != http.MethodPost {
		h.logger.Warn("method not allowed", zap.String("method", r.Method))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check circuit breaker - reject requests if circuit is open
	if h.circuitBreaker != nil && !h.circuitBreaker.CanAttempt() {
		h.logger.Warn("circuit breaker open, rejecting request",
			zap.String("remote_addr", r.RemoteAddr))
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"error":       "Service temporarily unavailable due to delivery failures",
			"retry_after": "60",
		})
		return
	}

	// Limit request body size to prevent DoS attacks
	r.Body = http.MaxBytesReader(w, r.Body, h.httpConfig.MaxBodySizeBytes)

	// Check authentication
	if h.httpConfig.AuthToken != "" {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			h.logger.Warn("unauthorized request - missing auth header",
				zap.String("remote_addr", r.RemoteAddr))
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Validate Bearer token format
		if !strings.HasPrefix(authHeader, "Bearer ") {
			h.logger.Warn("unauthorized request - invalid auth format",
				zap.String("remote_addr", r.RemoteAddr))
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		// Use constant-time comparison to prevent timing attacks
		if subtle.ConstantTimeCompare([]byte(token), []byte(h.httpConfig.AuthToken)) != 1 {
			h.logger.Warn("unauthorized request - invalid token",
				zap.String("remote_addr", r.RemoteAddr))
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	// Parse request
	contentType := r.Header.Get("Content-Type")
	var msgReq MessageRequest

	switch contentType {
	case "application/json":
		if err := json.NewDecoder(r.Body).Decode(&msgReq); err != nil {
			h.logger.Error("failed to decode JSON",
				zap.Error(err),
				zap.String("remote_addr", r.RemoteAddr))
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
	case "application/x-www-form-urlencoded":
		if err := r.ParseForm(); err != nil {
			h.logger.Error("failed to parse form",
				zap.Error(err),
				zap.String("remote_addr", r.RemoteAddr))
			http.Error(w, "Invalid form data", http.StatusBadRequest)
			return
		}
		msgReq.From = r.FormValue("from")
		msgReq.To = r.FormValue("to")
		msgReq.Subject = r.FormValue("subject")
		msgReq.Text = r.FormValue("text")
		msgReq.HTML = r.FormValue("html")
	default:
		h.logger.Warn("unsupported content type",
			zap.String("content_type", contentType))
		http.Error(w, "Unsupported Content-Type", http.StatusUnsupportedMediaType)
		return
	}

	// Validate required fields
	if msgReq.From == "" || msgReq.To == "" || msgReq.Subject == "" {
		h.logger.Warn("missing required fields",
			zap.String("from", msgReq.From),
			zap.String("to", msgReq.To),
			zap.String("subject", msgReq.Subject))
		http.Error(w, "Missing required fields: from, to, subject", http.StatusBadRequest)
		return
	}

	if msgReq.Text == "" && msgReq.HTML == "" {
		h.logger.Warn("missing body content")
		http.Error(w, "Either text or html body is required", http.StatusBadRequest)
		return
	}

	// Extract domain from recipient
	toDomain := queue.ExtractDomain(msgReq.To)
	if toDomain == "" {
		h.logger.Warn("invalid recipient email",
			zap.String("to", msgReq.To))
		http.Error(w, "Invalid recipient email address", http.StatusBadRequest)
		return
	}

	// Check for idempotency key if enabled
	var idempotencyKey string
	if h.httpConfig.IdempotencyEnabled {
		idempotencyKey = r.Header.Get(h.httpConfig.IdempotencyHeader)

		if idempotencyKey != "" {
			// First check gossip cache for distributed idempotency (best-effort)
			if h.gossip != nil {
				if claimedBy := h.gossip.CheckIdempotencyKey(idempotencyKey); claimedBy != "" {
					// Key is claimed by another node - return 409 Conflict
					h.logger.Warn("duplicate request detected via gossip - claimed by another node",
						zap.String("idempotency_key", idempotencyKey),
						zap.String("claimed_by", claimedBy))

					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusConflict) // 409 = duplicate being processed elsewhere
					json.NewEncoder(w).Encode(map[string]string{
						"error":      "Duplicate request detected",
						"message":    "This request is currently being processed by another node",
						"claimed_by": claimedBy,
					})
					return
				}
			}

			// Then check local database for idempotency
			existing, err := h.queue.GetMessageByIdempotencyKey(idempotencyKey)
			if err != nil {
				h.logger.Error("failed to check idempotency key",
					zap.String("idempotency_key", idempotencyKey),
					zap.Error(err))
				// Don't fail the request, continue without idempotency
			} else if existing != nil {
				// Duplicate request - return existing message with 202 Accepted (idempotent response)
				h.logger.Info("duplicate request detected via database idempotency key",
					zap.String("idempotency_key", idempotencyKey),
					zap.String("existing_message_id", existing.MessageID),
					zap.String("status", string(existing.Status)))

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted) // 202 = idempotent response
				json.NewEncoder(w).Encode(EnqueueResponse{
					MessageID: existing.MessageID,
					Status:    string(existing.Status),
					QueuedAt:  existing.CreatedAt.Format(time.RFC3339),
				})
				return
			}
		}
	}

	// Generate message ID
	messageID := queue.GenerateMessageID()

	// Validate DKIM private key if provided
	if msgReq.DKIMPrivateKey != "" {
		keySize, err := dkim.ValidatePrivateKey(msgReq.DKIMPrivateKey)
		if err != nil {
			h.logger.Warn("invalid DKIM private key",
				zap.Error(err),
				zap.String("message_id", messageID))
			http.Error(w, fmt.Sprintf("Invalid DKIM private key: %v", err), http.StatusBadRequest)
			return
		}
		h.logger.Debug("DKIM key validated",
			zap.String("message_id", messageID),
			zap.Int("key_size_bits", keySize))
	}

	// Default DKIM domain to sender's domain if not specified
	dkimDomain := msgReq.DKIMDomain
	if msgReq.DKIMPrivateKey != "" && dkimDomain == "" {
		dkimDomain = dkim.ExtractDomainFromEmail(msgReq.From)
	}

	// Build MIME message
	rawMessage, err := h.buildRawMessage(&msgReq)
	if err != nil {
		h.logger.Error("failed to build message",
			zap.Error(err),
			zap.String("message_id", messageID))
		http.Error(w, "Failed to build message", http.StatusInternalServerError)
		return
	}

	// Create queued message
	queuedMsg := &queue.QueuedMessage{
		MessageID:      messageID,
		IdempotencyKey: idempotencyKey,
		FromAddr:       msgReq.From,
		ToAddr:         msgReq.To,
		ToDomain:       toDomain,
		Subject:        msgReq.Subject,
		RawMessage:     rawMessage,
		DKIMPrivateKey: msgReq.DKIMPrivateKey,
		DKIMSelector:   msgReq.DKIMSelector,
		DKIMDomain:     dkimDomain,
		ExpiresAt:      delivery.CalculateExpiresAt(time.Now(), h.deliveryConfig.MaxMessageAgeHours),
	}

	// Enqueue message
	if err := h.queue.Enqueue(queuedMsg); err != nil {
		h.logger.Error("failed to enqueue message",
			zap.Error(err),
			zap.String("message_id", messageID))
		http.Error(w, "Failed to enqueue message", http.StatusInternalServerError)
		return
	}

	// Broadcast idempotency key to cluster if gossip is enabled
	if h.gossip != nil && idempotencyKey != "" {
		if err := h.gossip.BroadcastIdempotencyKey(idempotencyKey); err != nil {
			h.logger.Warn("failed to broadcast idempotency key",
				zap.String("idempotency_key", idempotencyKey),
				zap.Error(err))
			// Don't fail the request - gossip is best-effort
		}
	}

	duration := time.Since(start)

	h.logger.Info("message enqueued",
		zap.String("message_id", messageID),
		zap.String("from", msgReq.From),
		zap.String("to", msgReq.To),
		zap.String("subject", msgReq.Subject),
		zap.Duration("duration", duration))

	// Return 200 OK with message ID
	response := EnqueueResponse{
		MessageID: messageID,
		Status:    "queued",
		QueuedAt:  time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

// buildRawMessage constructs a MIME message from the request
func (h *QueueMessageHandler) buildRawMessage(req *MessageRequest) ([]byte, error) {
	var buf bytes.Buffer

	// Create message headers
	var header mail.Header
	header.SetDate(time.Now())
	header.SetAddressList("From", []*mail.Address{{Address: req.From}})
	header.SetAddressList("To", []*mail.Address{{Address: req.To}})
	header.SetSubject(req.Subject)

	// Create message writer
	mw, err := mail.CreateWriter(&buf, header)
	if err != nil {
		return nil, fmt.Errorf("failed to create message writer: %w", err)
	}

	// Write body
	if req.HTML != "" && req.Text != "" {
		// Multipart/alternative
		iw, err := mw.CreateInline()
		if err != nil {
			return nil, fmt.Errorf("failed to create inline writer: %w", err)
		}

		// Text part
		var textHeader mail.InlineHeader
		textHeader.Set("Content-Type", "text/plain; charset=utf-8")
		textPart, err := iw.CreatePart(textHeader)
		if err != nil {
			return nil, fmt.Errorf("failed to create text part: %w", err)
		}
		textPart.Write([]byte(req.Text))
		textPart.Close()

		// HTML part
		var htmlHeader mail.InlineHeader
		htmlHeader.Set("Content-Type", "text/html; charset=utf-8")
		htmlPart, err := iw.CreatePart(htmlHeader)
		if err != nil {
			return nil, fmt.Errorf("failed to create html part: %w", err)
		}
		htmlPart.Write([]byte(req.HTML))
		htmlPart.Close()

		iw.Close()
	} else if req.HTML != "" {
		// HTML only
		var htmlHeader mail.InlineHeader
		htmlHeader.Set("Content-Type", "text/html; charset=utf-8")
		htmlPart, err := mw.CreateSingleInline(htmlHeader)
		if err != nil {
			return nil, fmt.Errorf("failed to create html part: %w", err)
		}
		htmlPart.Write([]byte(req.HTML))
		htmlPart.Close()
	} else {
		// Text only
		var textHeader mail.InlineHeader
		textHeader.Set("Content-Type", "text/plain; charset=utf-8")
		textPart, err := mw.CreateSingleInline(textHeader)
		if err != nil {
			return nil, fmt.Errorf("failed to create text part: %w", err)
		}
		textPart.Write([]byte(req.Text))
		textPart.Close()
	}

	mw.Close()

	return buf.Bytes(), nil
}
