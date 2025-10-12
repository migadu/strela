// Package handler provides HTTP API endpoints for Fune's message submission and management.
//
// The handler package implements:
//   - Message submission API (POST /v1/messages) with JSON and form-encoded support
//   - Bearer token authentication with constant-time comparison
//   - Rate limiting per IP address with token bucket algorithm
//   - Circuit breaker integration to reject requests during delivery failures
//   - Distributed idempotency via gossip protocol (cluster mode)
//   - Request validation and MIME message construction
//   - Security headers middleware (X-Content-Type-Options, X-Frame-Options, etc.)
//   - Health check endpoints with comprehensive system status
//   - Admin cluster status endpoints
//   - Prometheus metrics middleware
//
// Request Flow:
//
//	Client → Rate Limiter → Auth Check → Circuit Breaker Check
//	→ Idempotency Check (Gossip + Database) → Request Validation
//	→ MIME Message Construction → Queue Enqueue → 202 Accepted Response
//
// The API is designed to be asynchronous: all requests return 202 Accepted immediately
// after queueing. Delivery status is communicated via webhook callbacks.
//
// Security Features:
//   - Bearer token authentication (Authorization: Bearer <token>)
//   - Rate limiting per IP (configurable requests per window)
//   - Request body size limits (default 10MB)
//   - Constant-time token comparison to prevent timing attacks
//   - Security headers middleware (defense in depth)
//   - X-Forwarded-For and X-Real-IP header support for proxied requests
//
// Idempotency:
//   - Two-level idempotency: gossip protocol (cluster-wide) + database (persistent)
//   - Custom header support (default: Idempotency-Key)
//   - Returns 409 Conflict if request is being processed by another node
//   - Returns 202 Accepted with existing message ID if already completed
//
// Circuit Breaker Integration:
//   - Rejects requests with 503 Service Unavailable when delivery circuit is open
//   - Prevents accepting messages that cannot be delivered
//   - Includes retry-after header for client backoff
package handler

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"fune/internal/config"
	"fune/internal/delivery"
	"fune/internal/dkim"
	"fune/internal/queue"
	"fune/internal/recovery"

	"github.com/emersion/go-message/mail"
)

// GossipService defines the interface for distributed idempotency via gossip protocol.
// Implementations must support broadcasting idempotency keys to cluster nodes and
// checking if a key has been claimed by another node.
//
// This interface is used for best-effort distributed idempotency coordination:
//   - BroadcastIdempotencyKey announces a key to the cluster after queuing
//   - CheckIdempotencyKey verifies if a key is claimed by another node before queuing
//
// The gossip protocol provides eventual consistency for idempotency across cluster nodes.
// Database idempotency checks provide authoritative persistent deduplication.
type GossipService interface {
	// BroadcastIdempotencyKey broadcasts an idempotency key to all cluster nodes.
	// This is called after successfully enqueuing a message to prevent duplicates
	// on other nodes. Returns an error if the broadcast fails (logged but not fatal).
	BroadcastIdempotencyKey(key string) error

	// CheckIdempotencyKey checks if an idempotency key has been claimed by another node.
	// Returns the node ID that claimed the key, or empty string if unclaimed.
	// This provides fast distributed deduplication before database queries.
	CheckIdempotencyKey(key string) string
}

// QueueMessageHandler handles HTTP message submission requests and enqueues them for delivery.
//
// This is the primary HTTP handler for Fune's API. It implements:
//   - Request parsing (JSON and form-encoded)
//   - Authentication via Bearer token
//   - Rate limiting per client IP
//   - Circuit breaker checks
//   - Two-level idempotency (gossip + database)
//   - MIME message construction
//   - DKIM signature validation
//   - Message queueing with immediate 202 Accepted response
//
// The handler returns 202 Accepted immediately after queueing, making the API fully asynchronous.
// Delivery happens in background workers, with results communicated via webhook callbacks.
//
// Thread Safety:
//
// QueueMessageHandler is safe for concurrent use by multiple goroutines. The underlying
// queue, rate limiter, and circuit breaker are all thread-safe.
//
// Example Usage:
//
//	handler := NewQueueMessageHandler(queue, deliveryCfg, httpCfg, circuitBreaker, logger)
//	if gossip != nil {
//	    handler.SetGossip(gossip) // Enable distributed idempotency
//	}
//	http.Handle("/v1/messages", handler)
type QueueMessageHandler struct {
	queue          *queue.Queue
	deliveryConfig *config.OutboundConfig
	httpConfig     *config.InboundConfig
	circuitBreaker *delivery.CircuitBreaker
	gossip         GossipService
	rateLimiter    *RateLimiter
	logger         *slog.Logger
}

// NewQueueMessageHandler creates a new HTTP handler for message submission.
//
// Parameters:
//   - q: Queue for message persistence and retrieval
//   - deliveryCfg: Outbound delivery configuration (used for message expiration calculation)
//   - httpCfg: Inbound HTTP API configuration (auth token, rate limits, body size, etc.)
//   - circuitBreaker: Delivery circuit breaker for rejecting requests during delivery failures
//   - logger: Structured logger for request/error logging
//
// The handler automatically initializes rate limiting if enabled in httpCfg.
// Gossip service must be set separately via SetGossip() after construction.
//
// Example:
//
//	handler := NewQueueMessageHandler(
//	    queue,
//	    &config.OutboundConfig{MaxMessageAgeHours: 48},
//	    &config.InboundConfig{
//	        AuthToken:            "secret-token",
//	        RateLimitEnabled:     true,
//	        RateLimitRequestsPerIP: 100,
//	        RateLimitWindowSeconds: 60,
//	    },
//	    circuitBreaker,
//	    logger,
//	)
func NewQueueMessageHandler(q *queue.Queue, deliveryCfg *config.OutboundConfig, httpCfg *config.InboundConfig, circuitBreaker *delivery.CircuitBreaker, logger *slog.Logger) *QueueMessageHandler {
	var rateLimiter *RateLimiter
	if httpCfg.RateLimitEnabled {
		rateLimiter = NewRateLimiter(httpCfg.RateLimitRequestsPerIP, httpCfg.RateLimitWindowSeconds)
		logger.Info("rate limiting enabled",
			"requests_per_ip", httpCfg.RateLimitRequestsPerIP,
			"window_seconds", httpCfg.RateLimitWindowSeconds)
	}

	return &QueueMessageHandler{
		queue:          q,
		deliveryConfig: deliveryCfg,
		httpConfig:     httpCfg,
		circuitBreaker: circuitBreaker,
		gossip:         nil, // Set via SetGossip() after initialization
		rateLimiter:    rateLimiter,
		logger:         logger,
	}
}

// SetGossip configures the gossip service for distributed idempotency across cluster nodes.
//
// This method must be called after handler creation if cluster mode is enabled. The gossip
// service enables:
//   - Broadcasting idempotency keys after successful message queueing
//   - Checking if an idempotency key has been claimed by another node
//
// Without gossip, idempotency is still enforced via database checks, but duplicate requests
// may briefly queue on multiple nodes before database deduplication occurs.
//
// Example:
//
//	handler := NewQueueMessageHandler(...)
//	gossip, _ := gossip.NewGossip(...)
//	handler.SetGossip(gossip) // Enable distributed idempotency
func (h *QueueMessageHandler) SetGossip(gossip GossipService) {
	h.gossip = gossip
}

// ServeHTTP handles incoming HTTP message submission requests.
//
// This method implements the http.Handler interface and processes POST requests to
// submit email messages for delivery. The request flow is:
//
//  1. Method validation (must be POST)
//  2. Rate limiting check (per client IP)
//  3. Circuit breaker check (reject if delivery circuit is open)
//  4. Request body size limit enforcement (MaxBodySizeBytes)
//  5. Bearer token authentication (if configured)
//  6. Content-Type parsing (application/json or application/x-www-form-urlencoded)
//  7. Request validation (required fields: from, to, subject, text/html)
//  8. Idempotency check (gossip + database)
//  9. DKIM key validation (if provided)
//  10. MIME message construction
//  11. Message queueing
//  12. Idempotency key broadcast (if gossip enabled)
//  13. 202 Accepted response with message ID
//
// Supported Content Types:
//   - application/json: JSON request body with MessageRequest fields
//   - application/x-www-form-urlencoded: Form-encoded request body
//
// Authentication:
//
// If httpConfig.AuthToken is set, requests must include:
//
//	Authorization: Bearer <token>
//
// Token comparison uses constant-time comparison to prevent timing attacks.
//
// Rate Limiting:
//
// If rate limiting is enabled, requests exceeding the configured limit return:
//
//	429 Too Many Requests
//	{"error": "API Rate Limit Exceeded"}
//
// Circuit Breaker:
//
// If the delivery circuit breaker is open, requests are rejected with:
//
//	503 Service Unavailable
//	{"error": "Service temporarily unavailable due to delivery failures", "retry_after": "60"}
//
// Idempotency:
//
// Clients can include an idempotency header (default: Idempotency-Key) to prevent duplicate
// message submission. Duplicate requests return:
//   - 409 Conflict if request is being processed by another cluster node
//   - 202 Accepted with existing message ID if request has completed
//
// Response:
//
// On success, returns 202 Accepted with JSON response:
//
//	{
//	    "message_id": "unique-message-id",
//	    "status": "queued",
//	    "queued_at": "2025-10-07T12:34:56Z"
//	}
//
// Error Responses:
//   - 400 Bad Request: Missing required fields, invalid email, invalid DKIM key, etc.
//   - 401 Unauthorized: Missing or invalid authentication token
//   - 405 Method Not Allowed: Non-POST request
//   - 409 Conflict: Duplicate request detected (another node processing)
//   - 413 Request Entity Too Large: Request body exceeds MaxBodySizeBytes
//   - 415 Unsupported Media Type: Invalid Content-Type
//   - 429 Too Many Requests: Rate limit exceeded
//   - 500 Internal Server Error: Failed to build message or enqueue
//   - 503 Service Unavailable: Circuit breaker open
func (h *QueueMessageHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer recovery.RecoverPanicWithCallback(h.logger, "HTTP handler", func(p interface{}) {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	})
	start := time.Now()

	// Log incoming request
	h.logger.Info("incoming request",
		"method", r.Method,
		"path", r.URL.Path,
		"remote_addr", r.RemoteAddr,
		"content_type", r.Header.Get("Content-Type"))

	if r.Method != http.MethodPost {
		h.logger.Warn("method not allowed", "method", r.Method)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check rate limit
	if h.rateLimiter != nil && !h.rateLimiter.Allow(r) {
		h.logger.Warn("rate limit exceeded",
			"remote_addr", r.RemoteAddr,
			"path", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "API Rate Limit Exceeded",
		})
		return
	}

	// Check circuit breaker - reject requests if circuit is open
	if h.circuitBreaker != nil && !h.circuitBreaker.CanAttempt() {
		h.logger.Warn("circuit breaker open, rejecting request",
			"remote_addr", r.RemoteAddr)
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
				"remote_addr", r.RemoteAddr)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Validate Bearer token format
		if !strings.HasPrefix(authHeader, "Bearer ") {
			h.logger.Warn("unauthorized request - invalid auth format",
				"remote_addr", r.RemoteAddr)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		// Use constant-time comparison to prevent timing attacks
		if subtle.ConstantTimeCompare([]byte(token), []byte(h.httpConfig.AuthToken)) != 1 {
			h.logger.Warn("unauthorized request - invalid token",
				"remote_addr", r.RemoteAddr)
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
				"error", err,
				"remote_addr", r.RemoteAddr)
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
	case "application/x-www-form-urlencoded":
		if err := r.ParseForm(); err != nil {
			h.logger.Error("failed to parse form",
				"error", err,
				"remote_addr", r.RemoteAddr)
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
			"content_type", contentType)
		http.Error(w, "Unsupported Content-Type", http.StatusUnsupportedMediaType)
		return
	}

	// Validate required fields
	if msgReq.From == "" || msgReq.To == "" || msgReq.Subject == "" {
		h.logger.Warn("missing required fields",
			"from", msgReq.From,
			"to", msgReq.To,
			"subject", msgReq.Subject)
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
			"to", msgReq.To)
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
						"idempotency_key", idempotencyKey,
						"claimed_by", claimedBy)

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
					"idempotency_key", idempotencyKey,
					"error", err)
				// Don't fail the request, continue without idempotency
			} else if existing != nil {
				// Duplicate request - return existing message with 202 Accepted (idempotent response)
				h.logger.Info("duplicate request detected via database idempotency key",
					"idempotency_key", idempotencyKey,
					"existing_message_id", existing.MessageID,
					"status", string(existing.Status))

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
				"error", err,
				"message_id", messageID)
			http.Error(w, fmt.Sprintf("Invalid DKIM private key: %v", err), http.StatusBadRequest)
			return
		}
		h.logger.Debug("DKIM key validated",
			"message_id", messageID,
			"key_size_bits", keySize)
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
			"error", err,
			"message_id", messageID)
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
			"error", err,
			"message_id", messageID)
		http.Error(w, "Failed to enqueue message", http.StatusInternalServerError)
		return
	}

	// Broadcast idempotency key to cluster if gossip is enabled
	if h.gossip != nil && idempotencyKey != "" {
		if err := h.gossip.BroadcastIdempotencyKey(idempotencyKey); err != nil {
			h.logger.Warn("failed to broadcast idempotency key",
				"idempotency_key", idempotencyKey,
				"error", err)
			// Don't fail the request - gossip is best-effort
		}
	}

	duration := time.Since(start)

	h.logger.Info("message enqueued",
		"message_id", messageID,
		"from", msgReq.From,
		"to", msgReq.To,
		"subject", msgReq.Subject,
		"duration", duration)

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

// buildRawMessage constructs a RFC 5322 MIME message from a MessageRequest.
//
// This method generates a properly formatted MIME email message with:
//   - Standard email headers (From, To, Subject, Date)
//   - UTF-8 encoded content
//   - Multipart/alternative structure (if both text and HTML provided)
//   - Single text/plain or text/html part (if only one provided)
//
// Message Structure:
//
// If both text and HTML are provided:
//
//	multipart/alternative
//	├── text/plain (UTF-8)
//	└── text/html (UTF-8)
//
// If only one body type is provided, a single inline part is created.
//
// The generated message is stored in the queue and will be DKIM-signed (if configured)
// before SMTP delivery.
//
// Returns the raw MIME message bytes, or an error if message construction fails.
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
