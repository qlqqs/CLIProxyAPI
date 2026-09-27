package codex

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// OAuthServer handles the local HTTP server for OAuth callbacks.
// It listens for the authorization code response from the OAuth provider
// and captures the necessary parameters to complete the authentication flow.
type OAuthServer struct {
	// server is the underlying HTTP server instance
	server *http.Server
	// port is the port number on which the server listens
	port int
	// expectedState binds every callback, including errors, to this login attempt.
	expectedState string
	// resultChan is a channel for sending OAuth results
	resultChan chan *OAuthResult
	// errorChan is a channel for sending OAuth errors
	errorChan chan error
	// mu is a mutex for protecting server state
	mu sync.Mutex
	// running indicates whether the server is currently running
	running bool
}

// OAuthResult contains the result of the OAuth callback.
// It holds either the authorization code and state for successful authentication
// or an error message if the authentication failed.
type OAuthResult struct {
	// Code is the authorization code received from the OAuth provider
	Code string
	// State is the state parameter used to prevent CSRF attacks
	State string
	// Error contains any error message if the OAuth flow failed
	Error string
}

// NewOAuthServer creates a new OAuth callback server.
// It initializes the server with the specified port and creates channels
// for handling OAuth results and errors.
//
// Parameters:
//   - port: The port number on which the server should listen
//   - expectedState: The unpredictable state belonging to this login attempt
//
// Returns:
//   - *OAuthServer: A new OAuthServer instance
func NewOAuthServer(port int, expectedState string) *OAuthServer {
	return &OAuthServer{
		port:          port,
		expectedState: expectedState,
		resultChan:    make(chan *OAuthResult, 1),
		errorChan:     make(chan error, 1),
	}
}

// Start starts the OAuth callback server.
// It sets up the HTTP handlers for the callback and success endpoints,
// and begins listening on the specified port.
//
// Returns:
//   - error: An error if the server fails to start
func (s *OAuthServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return fmt.Errorf("server is already running")
	}

	if s.expectedState == "" {
		return fmt.Errorf("OAuth callback state is required")
	}
	listener, errListen := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", s.port))
	if errListen != nil {
		return fmt.Errorf("listen on OAuth callback port %d: %w", s.port, errListen)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", s.handleCallback)
	mux.HandleFunc("/success", s.handleSuccess)

	s.server = &http.Server{
		Addr:         listener.Addr().String(),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	s.running = true

	// Capture the server so Stop cannot replace it before this goroutine starts.
	server := s.server
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.errorChan <- fmt.Errorf("server failed to start: %w", err)
		}
	}()

	return nil
}

// Stop gracefully stops the OAuth callback server.
// It performs a graceful shutdown of the HTTP server with a timeout.
//
// Parameters:
//   - ctx: The context for controlling the shutdown process
//
// Returns:
//   - error: An error if the server fails to stop gracefully
func (s *OAuthServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running || s.server == nil {
		return nil
	}

	log.Debug("Stopping OAuth callback server")

	// Create a context with timeout for shutdown
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	err := s.server.Shutdown(shutdownCtx)
	s.running = false
	s.server = nil

	return err
}

// WaitForCallback waits for the OAuth callback with a timeout.
// It blocks until either an OAuth result is received, an error occurs,
// or the specified timeout is reached.
//
// Parameters:
//   - timeout: The maximum time to wait for the callback
//
// Returns:
//   - *OAuthResult: The OAuth result if successful
//   - error: An error if the callback times out or an error occurs
func (s *OAuthServer) WaitForCallback(timeout time.Duration) (*OAuthResult, error) {
	select {
	case result := <-s.resultChan:
		return result, nil
	case err := <-s.errorChan:
		return nil, err
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for OAuth callback")
	}
}

// handleCallback handles the OAuth callback endpoint.
// It extracts the authorization code and state from the callback URL,
// validates the parameters, and sends the result to the waiting channel.
//
// Parameters:
//   - w: The HTTP response writer
//   - r: The HTTP request
func (s *OAuthServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	log.Debug("Received OAuth callback")

	// Validate request method
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract parameters
	query := r.URL.Query()
	code := query.Get("code")
	state := query.Get("state")
	errorParam := query.Get("error")

	// Reject unrelated callbacks without consuming the pending login's result.
	if s.expectedState == "" || len(query["state"]) != 1 || subtle.ConstantTimeCompare([]byte(state), []byte(s.expectedState)) != 1 {
		http.Error(w, "Invalid OAuth state", http.StatusBadRequest)
		return
	}
	if errorParam != "" {
		log.Warn("OAuth authorization was rejected")
		s.sendResult(&OAuthResult{State: state, Error: errorParam})
		http.Error(w, "OAuth authorization failed", http.StatusBadRequest)
		return
	}
	if code == "" {
		http.Error(w, "No authorization code received", http.StatusBadRequest)
		return
	}

	// Send successful result
	result := &OAuthResult{
		Code:  code,
		State: state,
	}
	s.sendResult(result)

	// Redirect to success page
	http.Redirect(w, r, "/success", http.StatusFound)
}

// handleSuccess handles the success page endpoint.
// It serves a user-friendly HTML page indicating that authentication was successful.
//
// Parameters:
//   - w: The HTTP response writer
//   - r: The HTTP request
func (s *OAuthServer) handleSuccess(w http.ResponseWriter, r *http.Request) {
	log.Debug("Serving success page")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	// Parse query parameters for customization
	query := r.URL.Query()
	setupRequired := query.Get("setup_required") == "true"
	platformURL := query.Get("platform_url")
	if platformURL == "" {
		platformURL = "https://platform.openai.com"
	}

	// Generate success page HTML with dynamic content
	successHTML := s.generateSuccessHTML(setupRequired, platformURL)

	_, err := w.Write([]byte(successHTML))
	if err != nil {
		log.Errorf("Failed to write success page: %v", err)
	}
}

// generateSuccessHTML creates the HTML content for the success page.
// It customizes the page based on whether additional setup is required
// and includes a link to the platform.
//
// Parameters:
//   - setupRequired: Whether additional setup is required after authentication
//   - platformURL: The URL to the platform for additional setup
//
// Returns:
//   - string: The HTML content for the success page
func (s *OAuthServer) generateSuccessHTML(setupRequired bool, platformURL string) string {
	parsed, errParse := url.Parse(strings.TrimSpace(platformURL))
	if errParse != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "platform.openai.com") || parsed.User != nil {
		platformURL = "https://platform.openai.com"
	} else {
		platformURL = parsed.String()
	}
	escapedPlatformURL := html.EscapeString(platformURL)
	page := strings.ReplaceAll(LoginSuccessHtml, "{{PLATFORM_URL}}", escapedPlatformURL)
	if setupRequired {
		setupNotice := strings.ReplaceAll(SetupNoticeHtml, "{{PLATFORM_URL}}", escapedPlatformURL)
		page = strings.Replace(page, "{{SETUP_NOTICE}}", setupNotice, 1)
	} else {
		page = strings.Replace(page, "{{SETUP_NOTICE}}", "", 1)
	}
	return page
}

// sendResult sends the OAuth result to the waiting channel.
// It ensures that the result is sent without blocking the handler.
//
// Parameters:
//   - result: The OAuth result to send
func (s *OAuthServer) sendResult(result *OAuthResult) {
	select {
	case s.resultChan <- result:
		log.Debug("OAuth result sent to channel")
	default:
		log.Warn("OAuth result channel is full, result dropped")
	}
}

// IsRunning returns whether the server is currently running.
//
// Returns:
//   - bool: True if the server is running, false otherwise
func (s *OAuthServer) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}
