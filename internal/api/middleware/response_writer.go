// Package middleware provides Gin HTTP middleware for the CLI Proxy API server.
// It includes a sophisticated response writer wrapper designed to capture and log request and response data,
// including support for streaming responses, without impacting latency.
package middleware

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/tidwall/gjson"
)

const requestBodyOverrideContextKey = "REQUEST_BODY_OVERRIDE"
const responseBodyOverrideContextKey = "RESPONSE_BODY_OVERRIDE"
const websocketTimelineOverrideContextKey = "WEBSOCKET_TIMELINE_OVERRIDE"

var requestSummaryUserIDHeaders = []string{
	"X-NewAPI-User-ID",
	"X-NewAPI-UserId",
	"X-NewAPI-User-Id",
	"NewAPI-User-ID",
	"NewAPI-UserId",
	"NewAPI-User-Id",
	"X-New-Api-User-Id",
	"X-New-Api-User-ID",
	"New-Api-User-Id",
	"New-Api-User-ID",
	"X-OneAPI-User-ID",
	"X-OneAPI-UserId",
	"X-OneAPI-User-Id",
	"OneAPI-User-ID",
	"OneAPI-UserId",
	"OneAPI-User-Id",
	"X-One-API-User-Id",
	"One-API-User-Id",
	"X-User-ID",
	"X-User-Id",
	"X-UserId",
	"X-Consumer-ID",
	"X-Consumer-Id",
	"X-Authenticated-User-ID",
	"X-Authenticated-User-Id",
}

var requestSummaryUsernameHeaders = []string{
	"X-NewAPI-Username",
	"X-NewAPI-UserName",
	"X-NewAPI-User-Name",
	"X-NewAPI-User",
	"X-NewAPI-Name",
	"NewAPI-Username",
	"NewAPI-UserName",
	"NewAPI-User-Name",
	"NewAPI-User",
	"NewAPI-Name",
	"X-New-Api-Username",
	"X-New-Api-UserName",
	"X-New-Api-User-Name",
	"X-New-Api-User",
	"X-New-Api-Name",
	"New-Api-Username",
	"New-Api-UserName",
	"New-Api-User-Name",
	"New-Api-User",
	"New-Api-Name",
	"X-OneAPI-Username",
	"X-OneAPI-UserName",
	"X-OneAPI-User-Name",
	"X-OneAPI-User",
	"OneAPI-Username",
	"OneAPI-UserName",
	"OneAPI-User-Name",
	"OneAPI-User",
	"X-One-API-Username",
	"X-One-API-UserName",
	"X-One-API-User-Name",
	"X-One-API-User",
	"One-API-Username",
	"One-API-UserName",
	"One-API-User-Name",
	"One-API-User",
	"X-Username",
	"X-User-Name",
	"X-UserName",
	"X-User",
	"X-Forwarded-User",
	"X-Authenticated-User",
	"X-Consumer-Username",
	"X-Consumer-User",
	"X-Login-Name",
	"X-Account-Name",
}

// RequestInfo holds essential details of an incoming HTTP request for logging purposes.
type RequestInfo struct {
	URL       string              // URL is the request URL.
	Method    string              // Method is the HTTP method (e.g., GET, POST).
	Headers   map[string][]string // Headers contains the request headers.
	Body      []byte              // Body is the raw request body.
	RequestID string              // RequestID is the unique identifier for the request.
	Timestamp time.Time           // Timestamp is when the request was received.
}

// ResponseWriterWrapper wraps the standard gin.ResponseWriter to intercept and log response data.
// It is designed to handle both standard and streaming responses, ensuring that logging operations do not block the client response.
type ResponseWriterWrapper struct {
	gin.ResponseWriter
	body                *bytes.Buffer              // body is a buffer to store the response body for non-streaming responses.
	isStreaming         bool                       // isStreaming indicates whether the response is a streaming type (e.g., text/event-stream).
	streamWriter        logging.StreamingLogWriter // streamWriter is a writer for handling streaming log entries.
	chunkChannel        chan []byte                // chunkChannel is a channel for asynchronously passing response chunks to the logger.
	streamDone          chan struct{}              // streamDone signals when the streaming goroutine completes.
	logger              logging.RequestLogger      // logger is the instance of the request logger service.
	requestInfo         *RequestInfo               // requestInfo holds the details of the original request.
	statusCode          int                        // statusCode stores the HTTP status code of the response.
	headers             map[string][]string        // headers stores the response headers.
	logOnErrorOnly      bool                       // logOnErrorOnly enables logging only when an error response is detected.
	firstChunkTimestamp time.Time                  // firstChunkTimestamp captures TTFB for streaming responses.
}

// NewResponseWriterWrapper creates and initializes a new ResponseWriterWrapper.
// It takes the original gin.ResponseWriter, a logger instance, and request information.
//
// Parameters:
//   - w: The original gin.ResponseWriter to wrap.
//   - logger: The logging service to use for recording requests.
//   - requestInfo: The pre-captured information about the incoming request.
//
// Returns:
//   - A pointer to a new ResponseWriterWrapper.
func NewResponseWriterWrapper(w gin.ResponseWriter, logger logging.RequestLogger, requestInfo *RequestInfo) *ResponseWriterWrapper {
	return &ResponseWriterWrapper{
		ResponseWriter: w,
		body:           &bytes.Buffer{},
		logger:         logger,
		requestInfo:    requestInfo,
		headers:        make(map[string][]string),
	}
}

// Write wraps the underlying ResponseWriter's Write method to capture response data.
// For non-streaming responses, it writes to an internal buffer. For streaming responses,
// it sends data chunks to a non-blocking channel for asynchronous logging.
// CRITICAL: This method prioritizes writing to the client to ensure zero latency,
// handling logging operations subsequently.
func (w *ResponseWriterWrapper) Write(data []byte) (int, error) {
	// Ensure headers are captured before first write
	// This is critical because Write() may trigger WriteHeader() internally
	w.ensureHeadersCaptured()

	// CRITICAL: Write to client first (zero latency)
	n, err := w.ResponseWriter.Write(data)

	// THEN: Handle logging based on response type
	if w.isStreaming && w.chunkChannel != nil {
		// Capture TTFB on first chunk (synchronous, before async channel send)
		if w.firstChunkTimestamp.IsZero() {
			w.firstChunkTimestamp = time.Now()
		}
		// For streaming responses: Send to async logging channel (non-blocking)
		select {
		case w.chunkChannel <- append([]byte(nil), data...): // Non-blocking send with copy
		default: // Channel full, skip logging to avoid blocking
		}
		return n, err
	}

	if w.shouldBufferResponseBody() {
		w.body.Write(data)
	}

	return n, err
}

func (w *ResponseWriterWrapper) shouldBufferResponseBody() bool {
	if w.logger != nil && w.logger.IsEnabled() {
		return true
	}
	if !w.logOnErrorOnly {
		return false
	}
	status := w.statusCode
	if status == 0 {
		if statusWriter, ok := w.ResponseWriter.(interface{ Status() int }); ok && statusWriter != nil {
			status = statusWriter.Status()
		} else {
			status = http.StatusOK
		}
	}
	return status >= http.StatusBadRequest
}

// WriteString wraps the underlying ResponseWriter's WriteString method to capture response data.
// Some handlers (and fmt/io helpers) write via io.StringWriter; without this override, those writes
// bypass Write() and would be missing from request logs.
func (w *ResponseWriterWrapper) WriteString(data string) (int, error) {
	w.ensureHeadersCaptured()

	// CRITICAL: Write to client first (zero latency)
	n, err := w.ResponseWriter.WriteString(data)

	// THEN: Capture for logging
	if w.isStreaming && w.chunkChannel != nil {
		// Capture TTFB on first chunk (synchronous, before async channel send)
		if w.firstChunkTimestamp.IsZero() {
			w.firstChunkTimestamp = time.Now()
		}
		select {
		case w.chunkChannel <- []byte(data):
		default:
		}
		return n, err
	}

	if w.shouldBufferResponseBody() {
		w.body.WriteString(data)
	}
	return n, err
}

// WriteHeader wraps the underlying ResponseWriter's WriteHeader method.
// It captures the status code, detects if the response is streaming based on the Content-Type header,
// and initializes the appropriate logging mechanism (standard or streaming).
func (w *ResponseWriterWrapper) WriteHeader(statusCode int) {
	w.statusCode = statusCode

	// Capture response headers using the new method
	w.captureCurrentHeaders()

	// Detect streaming based on Content-Type
	contentType := w.ResponseWriter.Header().Get("Content-Type")
	w.isStreaming = w.detectStreaming(contentType)

	// If streaming, initialize streaming log writer
	if w.isStreaming && w.logger.IsEnabled() {
		streamWriter, err := w.logger.LogStreamingRequest(
			w.requestInfo.URL,
			w.requestInfo.Method,
			w.requestInfo.Headers,
			w.requestInfo.Body,
			w.requestInfo.RequestID,
		)
		if err == nil {
			w.streamWriter = streamWriter
			w.chunkChannel = make(chan []byte, 100) // Buffered channel for async writes
			doneChan := make(chan struct{})
			w.streamDone = doneChan

			// Start async chunk processor
			go w.processStreamingChunks(doneChan)

			// Write status immediately
			_ = streamWriter.WriteStatus(statusCode, w.headers)
		}
	}

	// Call original WriteHeader
	w.ResponseWriter.WriteHeader(statusCode)
}

// ensureHeadersCaptured is a helper function to make sure response headers are captured.
// It is safe to call this method multiple times; it will always refresh the headers
// with the latest state from the underlying ResponseWriter.
func (w *ResponseWriterWrapper) ensureHeadersCaptured() {
	// Always capture the current headers to ensure we have the latest state
	w.captureCurrentHeaders()
}

// captureCurrentHeaders reads all headers from the underlying ResponseWriter and stores them
// in the wrapper's headers map. It creates copies of the header values to prevent race conditions.
func (w *ResponseWriterWrapper) captureCurrentHeaders() {
	// Initialize headers map if needed
	if w.headers == nil {
		w.headers = make(map[string][]string)
	}

	// Capture all current headers from the underlying ResponseWriter
	for key, values := range w.ResponseWriter.Header() {
		// Make a copy of the values slice to avoid reference issues
		headerValues := make([]string, len(values))
		copy(headerValues, values)
		w.headers[key] = headerValues
	}
}

// detectStreaming determines if a response should be treated as a streaming response.
// It checks for a "text/event-stream" Content-Type or a '"stream": true'
// field in the original request body.
func (w *ResponseWriterWrapper) detectStreaming(contentType string) bool {
	// Check Content-Type for Server-Sent Events
	if strings.Contains(contentType, "text/event-stream") {
		return true
	}

	// If a concrete Content-Type is already set (e.g., application/json for error responses),
	// treat it as non-streaming instead of inferring from the request payload.
	if strings.TrimSpace(contentType) != "" {
		return false
	}

	// Only fall back to request payload hints when Content-Type is not set yet.
	if w.requestInfo != nil && len(w.requestInfo.Body) > 0 {
		return bytes.Contains(w.requestInfo.Body, []byte(`"stream": true`)) ||
			bytes.Contains(w.requestInfo.Body, []byte(`"stream":true`))
	}

	return false
}

// processStreamingChunks runs in a separate goroutine to process response chunks from the chunkChannel.
// It asynchronously writes each chunk to the streaming log writer.
func (w *ResponseWriterWrapper) processStreamingChunks(done chan struct{}) {
	if done == nil {
		return
	}

	defer close(done)

	if w.streamWriter == nil || w.chunkChannel == nil {
		return
	}

	for chunk := range w.chunkChannel {
		w.streamWriter.WriteChunkAsync(chunk)
	}
}

// Finalize completes the logging process for the request and response.
// For streaming responses, it closes the chunk channel and the stream writer.
// For non-streaming responses, it logs the complete request and response details,
// including any API-specific request/response data stored in the Gin context.
func (w *ResponseWriterWrapper) Finalize(c *gin.Context) error {
	if w.logger == nil {
		return nil
	}

	finalStatusCode := w.statusCode
	if finalStatusCode == 0 {
		if statusWriter, ok := w.ResponseWriter.(interface{ Status() int }); ok {
			finalStatusCode = statusWriter.Status()
		} else {
			finalStatusCode = 200
		}
	}

	var slicesAPIResponseError []*interfaces.ErrorMessage
	apiResponseError, isExist := c.Get("API_RESPONSE_ERROR")
	if isExist {
		if apiErrors, ok := apiResponseError.([]*interfaces.ErrorMessage); ok {
			slicesAPIResponseError = apiErrors
		}
	}

	hasAPIError := len(slicesAPIResponseError) > 0 || finalStatusCode >= http.StatusBadRequest
	forceLog := w.logOnErrorOnly && hasAPIError && !w.logger.IsEnabled()
	if !w.logger.IsEnabled() && !forceLog {
		return nil
	}

	w.populateRequestSummary(c, w.extractRequestBody(c), w.extractAPIResponse(c), w.extractResponseBody(c), w.extractAPIResponseTimestamp(c))

	if w.isStreaming && w.streamWriter != nil {
		if w.chunkChannel != nil {
			close(w.chunkChannel)
			w.chunkChannel = nil
		}

		if w.streamDone != nil {
			<-w.streamDone
			w.streamDone = nil
		}

		w.streamWriter.SetFirstChunkTimestamp(w.firstChunkTimestamp)

		// Write API Request and Response to the streaming log before closing
		apiRequest := w.extractAPIRequest(c)
		if len(apiRequest) > 0 {
			_ = w.streamWriter.WriteAPIRequest(apiRequest)
		}
		apiResponse := w.extractAPIResponse(c)
		if len(apiResponse) > 0 {
			_ = w.streamWriter.WriteAPIResponse(apiResponse)
		}
		apiWebsocketTimeline := w.extractAPIWebsocketTimeline(c)
		if len(apiWebsocketTimeline) > 0 {
			_ = w.streamWriter.WriteAPIWebsocketTimeline(apiWebsocketTimeline)
		}
		if err := w.streamWriter.Close(); err != nil {
			w.streamWriter = nil
			return err
		}
		w.streamWriter = nil
		return nil
	}

	return w.logRequest(w.extractRequestBody(c), finalStatusCode, w.cloneHeaders(), w.extractResponseBody(c), w.extractWebsocketTimeline(c), w.extractAPIRequest(c), w.extractAPIResponse(c), w.extractAPIWebsocketTimeline(c), w.extractAPIResponseTimestamp(c), slicesAPIResponseError, forceLog)
}

func (w *ResponseWriterWrapper) populateRequestSummary(c *gin.Context, requestBody []byte, apiResponse []byte, responseBody []byte, apiResponseTimestamp time.Time) {
	if c == nil || w.requestInfo == nil {
		return
	}

	sessionID := firstHeaderValue(w.requestInfo.Headers, "X-Session-ID")
	userID, username := parseRequestSummarySessionID(sessionID)
	if userID == "" {
		userID = firstHeaderValueAny(w.requestInfo.Headers, requestSummaryUserIDHeaders...)
	}
	if username == "" {
		username = firstHeaderValueAny(w.requestInfo.Headers, requestSummaryUsernameHeaders...)
	}
	if userID == "" || username == "" {
		metadataUserID, metadataUsername := requestSummaryIdentityFromTurnMetadata(firstHeaderValue(w.requestInfo.Headers, "X-Codex-Turn-Metadata"))
		if userID == "" {
			userID = metadataUserID
		}
		if username == "" {
			username = metadataUsername
		}
	}
	if username == "" && isReadableRequestSummarySessionName(sessionID) {
		username = sessionID
	}
	if username == "" {
		username = userID
	}
	logging.SetRequestSummaryValue(c, logging.RequestSummarySessionIDKey, sessionID)
	logging.SetRequestSummaryValue(c, logging.RequestSummaryUserIDKey, userID)
	logging.SetRequestSummaryValue(c, logging.RequestSummaryUsernameKey, username)
	logging.SetRequestSummaryValue(c, logging.RequestSummaryModelKey, firstNonEmptyJSONField(requestBody, "model", "model_name", "response.model"))
	logging.SetRequestSummaryValue(c, logging.RequestSummaryRequestTypeKey, inferRequestSummaryType(w.requestInfo.URL))

	if !apiResponseTimestamp.IsZero() && !w.requestInfo.Timestamp.IsZero() {
		firstByteMS := apiResponseTimestamp.Sub(w.requestInfo.Timestamp).Milliseconds()
		if firstByteMS >= 0 {
			logging.SetRequestSummaryValue(c, logging.RequestSummaryFirstByteMSKey, strconv.FormatInt(firstByteMS, 10))
		}
	}

	totalTokens := extractTotalTokens(apiResponse)
	if totalTokens == 0 {
		totalTokens = extractTotalTokens(responseBody)
	}
	if totalTokens > 0 {
		logging.SetRequestSummaryValue(c, logging.RequestSummaryTotalTokensKey, strconv.FormatInt(totalTokens, 10))
	}
}

func firstHeaderValue(headers map[string][]string, key string) string {
	if len(headers) == 0 {
		return ""
	}
	normalizedKey := normalizeRequestSummaryHeaderName(key)
	for currentKey, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(currentKey), key) &&
			normalizeRequestSummaryHeaderName(currentKey) != normalizedKey {
			continue
		}
		if len(values) == 0 {
			return ""
		}
		return strings.TrimSpace(values[0])
	}
	return ""
}

func normalizeRequestSummaryHeaderName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	var builder strings.Builder
	builder.Grow(len(name))
	for _, char := range name {
		if char == '-' || char == '_' || char == ' ' {
			continue
		}
		builder.WriteRune(unicode.ToLower(char))
	}
	return builder.String()
}

func firstHeaderValueAny(headers map[string][]string, keys ...string) string {
	for _, key := range keys {
		if value := firstHeaderValue(headers, key); value != "" {
			return value
		}
	}
	return ""
}

func requestSummaryIdentityFromTurnMetadata(raw string) (userID string, username string) {
	for _, candidate := range requestSummaryMetadataCandidates(raw) {
		if id, name := requestSummaryIdentityFromJSON(candidate); id != "" || name != "" {
			return id, name
		}
	}
	return "", ""
}

func requestSummaryMetadataCandidates(raw string) [][]byte {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	candidates := [][]byte{[]byte(raw)}
	if decoded, err := url.QueryUnescape(raw); err == nil && decoded != raw {
		candidates = append(candidates, []byte(strings.TrimSpace(decoded)))
	}
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if decoded, err := encoding.DecodeString(raw); err == nil && len(decoded) > 0 {
			candidates = append(candidates, decoded)
		}
	}
	return candidates
}

func requestSummaryIdentityFromJSON(data []byte) (userID string, username string) {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 || data[0] != '{' {
		return "", ""
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", ""
	}
	userID = firstRequestSummaryMetadataString(payload,
		"user_id", "userid", "userId", "uid", "id", "newapi_user_id", "newapiUserId",
		"user.id", "user.user_id", "user.userId", "user.uid", "account.id", "account.user_id",
	)
	username = firstRequestSummaryMetadataString(payload,
		"username", "user_name", "userName", "name", "login", "email", "newapi_username", "newapiUsername",
		"user.username", "user.user_name", "user.userName", "user.name", "user.login", "user.email",
		"account.username", "account.user_name", "account.name", "account.email",
	)
	return userID, username
}

func firstRequestSummaryMetadataString(payload map[string]any, paths ...string) string {
	for _, path := range paths {
		if value := requestSummaryMetadataValue(payload, strings.Split(path, ".")); value != "" {
			return value
		}
	}
	return ""
}

func requestSummaryMetadataValue(value any, parts []string) string {
	if len(parts) == 0 {
		switch typed := value.(type) {
		case string:
			return strings.TrimSpace(typed)
		case float64:
			if typed == float64(int64(typed)) {
				return strconv.FormatInt(int64(typed), 10)
			}
		case json.Number:
			return strings.TrimSpace(typed.String())
		}
		return ""
	}
	current, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	want := normalizeRequestSummaryMetadataKey(parts[0])
	for key, child := range current {
		if normalizeRequestSummaryMetadataKey(key) == want {
			return requestSummaryMetadataValue(child, parts[1:])
		}
	}
	return ""
}

func normalizeRequestSummaryMetadataKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	var builder strings.Builder
	builder.Grow(len(key))
	for _, char := range key {
		if char == '-' || char == '_' || char == ' ' {
			continue
		}
		builder.WriteRune(unicode.ToLower(char))
	}
	return builder.String()
}

func isReadableRequestSummarySessionName(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.Contains(sessionID, ":") || isUUIDLikeRequestSummarySessionID(sessionID) {
		return false
	}
	for _, char := range sessionID {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.' || char == '@' {
			continue
		}
		return false
	}
	return true
}

func isUUIDLikeRequestSummarySessionID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, char := range value {
		switch i {
		case 8, 13, 18, 23:
			if char != '-' {
				return false
			}
		default:
			if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f' || char >= 'A' && char <= 'F') {
				return false
			}
		}
	}
	return true
}

func parseRequestSummarySessionID(sessionID string) (userID string, username string) {
	trimmed := strings.TrimSpace(sessionID)
	if trimmed == "" {
		return "", ""
	}
	const prefix = "newapi-user-"
	lowered := strings.ToLower(trimmed)
	if !strings.HasPrefix(lowered, prefix) {
		return "", ""
	}
	raw := strings.TrimSpace(trimmed[len(prefix):])
	if raw == "" {
		return "", ""
	}
	parts := strings.SplitN(raw, "+", 2)
	userID = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		username = strings.TrimSpace(parts[1])
	}
	return userID, username
}

func firstNonEmptyJSONField(data []byte, paths ...string) string {
	if len(data) == 0 {
		return ""
	}
	for _, path := range paths {
		value := strings.TrimSpace(gjson.GetBytes(data, path).String())
		if value != "" {
			return value
		}
	}
	return ""
}

func inferRequestSummaryType(rawURL string) string {
	if strings.TrimSpace(rawURL) == "" {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	path := strings.Trim(parsed.Path, "/")
	if path == "" {
		return ""
	}
	segments := strings.Split(path, "/")
	switch {
	case len(segments) >= 2 && segments[0] == "v1":
		return strings.Join(segments[1:], "/")
	case len(segments) >= 3 && segments[0] == "api" && segments[1] == "provider":
		return strings.Join(segments[2:], "/")
	default:
		return strings.Join(segments, "/")
	}
}

func extractTotalTokens(data []byte) int64 {
	if len(data) == 0 {
		return 0
	}
	paths := []string{
		"usage.total_tokens",
		"usage.totalTokenCount",
		"usageMetadata.totalTokenCount",
		"usage_metadata.totalTokenCount",
		"response.usage.total_tokens",
		"response.usage.totalTokenCount",
		"response.usageMetadata.totalTokenCount",
		"response.usage_metadata.totalTokenCount",
	}
	for _, path := range paths {
		value := gjson.GetBytes(data, path)
		if value.Exists() {
			total := value.Int()
			if total > 0 {
				return total
			}
		}
	}
	return 0
}

func (w *ResponseWriterWrapper) cloneHeaders() map[string][]string {
	w.ensureHeadersCaptured()

	finalHeaders := make(map[string][]string, len(w.headers))
	for key, values := range w.headers {
		headerValues := make([]string, len(values))
		copy(headerValues, values)
		finalHeaders[key] = headerValues
	}

	return finalHeaders
}

func (w *ResponseWriterWrapper) extractAPIRequest(c *gin.Context) []byte {
	apiRequest, isExist := c.Get("API_REQUEST")
	if !isExist {
		return nil
	}
	data, ok := apiRequest.([]byte)
	if !ok || len(data) == 0 {
		return nil
	}
	return data
}

func (w *ResponseWriterWrapper) extractAPIResponse(c *gin.Context) []byte {
	apiResponse, isExist := c.Get("API_RESPONSE")
	if !isExist {
		return nil
	}
	data, ok := apiResponse.([]byte)
	if !ok || len(data) == 0 {
		return nil
	}
	return data
}

func (w *ResponseWriterWrapper) extractAPIWebsocketTimeline(c *gin.Context) []byte {
	apiTimeline, isExist := c.Get("API_WEBSOCKET_TIMELINE")
	if !isExist {
		return nil
	}
	data, ok := apiTimeline.([]byte)
	if !ok || len(data) == 0 {
		return nil
	}
	return bytes.Clone(data)
}

func (w *ResponseWriterWrapper) extractAPIResponseTimestamp(c *gin.Context) time.Time {
	ts, isExist := c.Get("API_RESPONSE_TIMESTAMP")
	if !isExist {
		return time.Time{}
	}
	if t, ok := ts.(time.Time); ok {
		return t
	}
	return time.Time{}
}

func (w *ResponseWriterWrapper) extractRequestBody(c *gin.Context) []byte {
	if body := extractBodyOverride(c, requestBodyOverrideContextKey); len(body) > 0 {
		return body
	}
	if w.requestInfo != nil && len(w.requestInfo.Body) > 0 {
		return w.requestInfo.Body
	}
	return nil
}

func (w *ResponseWriterWrapper) extractResponseBody(c *gin.Context) []byte {
	if body := extractBodyOverride(c, responseBodyOverrideContextKey); len(body) > 0 {
		return body
	}
	if w.body == nil || w.body.Len() == 0 {
		return nil
	}
	return bytes.Clone(w.body.Bytes())
}

func (w *ResponseWriterWrapper) extractWebsocketTimeline(c *gin.Context) []byte {
	return extractBodyOverride(c, websocketTimelineOverrideContextKey)
}

func extractBodyOverride(c *gin.Context, key string) []byte {
	if c == nil {
		return nil
	}
	bodyOverride, isExist := c.Get(key)
	if !isExist {
		return nil
	}
	switch value := bodyOverride.(type) {
	case []byte:
		if len(value) > 0 {
			return bytes.Clone(value)
		}
	case string:
		if strings.TrimSpace(value) != "" {
			return []byte(value)
		}
	}
	return nil
}

func (w *ResponseWriterWrapper) logRequest(requestBody []byte, statusCode int, headers map[string][]string, body, websocketTimeline, apiRequestBody, apiResponseBody, apiWebsocketTimeline []byte, apiResponseTimestamp time.Time, apiResponseErrors []*interfaces.ErrorMessage, forceLog bool) error {
	if w.requestInfo == nil {
		return nil
	}

	if loggerWithOptions, ok := w.logger.(interface {
		LogRequestWithOptions(string, string, map[string][]string, []byte, int, map[string][]string, []byte, []byte, []byte, []byte, []byte, []*interfaces.ErrorMessage, bool, string, time.Time, time.Time) error
	}); ok {
		return loggerWithOptions.LogRequestWithOptions(
			w.requestInfo.URL,
			w.requestInfo.Method,
			w.requestInfo.Headers,
			requestBody,
			statusCode,
			headers,
			body,
			websocketTimeline,
			apiRequestBody,
			apiResponseBody,
			apiWebsocketTimeline,
			apiResponseErrors,
			forceLog,
			w.requestInfo.RequestID,
			w.requestInfo.Timestamp,
			apiResponseTimestamp,
		)
	}

	return w.logger.LogRequest(
		w.requestInfo.URL,
		w.requestInfo.Method,
		w.requestInfo.Headers,
		requestBody,
		statusCode,
		headers,
		body,
		websocketTimeline,
		apiRequestBody,
		apiResponseBody,
		apiWebsocketTimeline,
		apiResponseErrors,
		w.requestInfo.RequestID,
		w.requestInfo.Timestamp,
		apiResponseTimestamp,
	)
}
