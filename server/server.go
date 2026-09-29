package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core/auth"
	"github.com/navidrome/navidrome/core/metrics"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/server/events"
	"github.com/navidrome/navidrome/ui"
)

type Server struct {
	router           chi.Router
	ds               model.DataStore
	appRoot          string
	broker           events.Broker
	insights         metrics.Insights
	rootCompatRoutes []rootCompatRoute
}

// rootCompatRoute describes a legacy API route that is reachable without the API's
// normal mount prefix. Some clients (notably SenPlayer configured against an Emby
// server) send /Users/... directly at the server root. Keep these aliases explicit
// so unrelated root paths continue to be handled by the web UI or their own router.
type rootCompatRoute struct {
	handler  http.Handler
	prefixes []string
}

func New(ds model.DataStore, broker events.Broker, insights metrics.Insights) *Server {
	s := &Server{ds: ds, broker: broker, insights: insights}
	initialSetup(ds)
	auth.Init(s.ds)
	s.initRoutes()
	s.mountAuthenticationRoutes()
	s.mountRootRedirector()
	checkFFmpegInstallation()
	checkExternalCredentials()
	return s
}

func (s *Server) MountRouter(description, urlPath string, subRouter http.Handler) {
	urlPath = path.Join(conf.Server.BasePath, urlPath)
	log.Info(fmt.Sprintf("Mounting %s routes", description), "path", urlPath)
	s.router.Group(func(r chi.Router) {
		r.Mount(urlPath, subRouter)
	})
}

// MountRouterWithRootPrefixes mounts a router at its normal URL path and exposes a
// deliberately small set of that router's paths at the configured BasePath root.
//
// The root aliases exist for clients that omit /jellyfin or /emby from their base
// URL. prefixes are path segments such as "users" or "items"; matching is
// case-insensitive and requires a segment boundary. The dispatched request is
// cloned with BasePath removed and with a fresh chi routing context, so the mounted
// router behaves exactly as it does under its regular mount point.
func (s *Server) MountRouterWithRootPrefixes(description, urlPath string, subRouter http.Handler, prefixes ...string) {
	s.MountRouter(description, urlPath, subRouter)

	normalized := normalizeRootCompatPrefixes(prefixes)
	if len(normalized) == 0 {
		return
	}

	s.rootCompatRoutes = append(s.rootCompatRoutes, rootCompatRoute{
		handler:  subRouter,
		prefixes: normalized,
	})
	log.Info(fmt.Sprintf("Mounting %s root compatibility routes", description),
		"prefixes", normalized)
}

// Run starts the server with the given address, and if specified, with TLS enabled.
func (s *Server) Run(ctx context.Context, addr string, port int, tlsCert string, tlsKey string) error {
	// Mount the router for the frontend assets
	s.MountRouter("WebUI", consts.URLPathUI, s.frontendAssetsHandler())

	// Create a new http.Server with the specified read header timeout and handler
	server := &http.Server{
		ReadHeaderTimeout: consts.ServerReadHeaderTimeout,
		Handler:           s.router,
	}

	// Determine if TLS is enabled
	tlsEnabled := tlsCert != "" && tlsKey != ""

	// Validate TLS certificates before starting the server
	if tlsEnabled {
		if err := validateTLSCertificates(tlsCert, tlsKey); err != nil {
			return err
		}
	}

	// Create a listener based on the address type (either Unix socket or TCP)
	var listener net.Listener
	var err error
	if after, ok := strings.CutPrefix(addr, "unix:"); ok {
		socketPath := after
		listener, err = createUnixSocketFile(socketPath, conf.Server.UnixSocketPerm)
		if err != nil {
			return err
		}
	} else {
		addr = fmt.Sprintf("%s:%d", addr, port)
		listener, err = net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("creating tcp listener: %w", err)
		}
	}

	// Start the server in a new goroutine and send an error signal to errC if there's an error
	errC := make(chan error)
	go func() {
		var err error
		if tlsEnabled {
			// Start the HTTPS server
			log.Info("Starting server with TLS (HTTPS) enabled", "tlsCert", tlsCert, "tlsKey", tlsKey)
			err = server.ServeTLS(listener, tlsCert, tlsKey)
		} else {
			// Start the HTTP server
			err = server.Serve(listener)
		}
		if !errors.Is(err, http.ErrServerClosed) {
			errC <- err
		}
	}()

	// Measure server startup time
	startupTime := time.Since(consts.ServerStart)

	// Wait a short time to make sure the server has started successfully
	select {
	case err := <-errC:
		log.Error(ctx, "Could not start server. Aborting", err)
		return fmt.Errorf("starting server: %w", err)
	case <-time.After(50 * time.Millisecond):
		log.Info(ctx, "----> Navidrome server is ready!", "address", addr, "startupTime", startupTime, "tlsEnabled", tlsEnabled)
	}

	// Wait for a signal to terminate
	select {
	case err := <-errC:
		return fmt.Errorf("running server: %w", err)
	case <-ctx.Done():
		// If the context is done (i.e. the server should stop), proceed to shutting down the server
	}

	// Try to stop the HTTP server gracefully
	log.Info(ctx, "Stopping HTTP server")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	server.SetKeepAlivesEnabled(false)
	if err := server.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Error(ctx, "Unexpected error in http.Shutdown()", err)
	}
	return nil
}

func createUnixSocketFile(socketPath string, socketPerm string) (net.Listener, error) {
	// Remove the socket file if it already exists
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("removing previous unix socket file: %w", err)
	}
	// Create listener
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("creating unix socket listener: %w", err)
	}
	// Converts the socketPerm to uint and updates the permission of the unix socket file
	perm, err := strconv.ParseUint(socketPerm, 8, 32)
	if err != nil {
		return nil, fmt.Errorf("parsing unix socket file permissions: %w", err)
	}
	err = os.Chmod(socketPath, os.FileMode(perm))
	if err != nil {
		return nil, fmt.Errorf("updating permission of unix socket file: %w", err)
	}
	return listener, nil
}

func (s *Server) initRoutes() {
	s.appRoot = path.Join(conf.Server.BasePath, consts.URLPathUI)

	r := chi.NewRouter()

	defaultMiddlewares := chi.Middlewares{
		secureMiddleware(),
		corsHandler(),
		middleware.RequestID,
		realIPMiddleware,
		middleware.Recoverer,
		middleware.Heartbeat("/ping"),
		robotsTXT(ui.BuildAssets()),
		serverAddressMiddleware,
		clientUniqueIDMiddleware,
		compressMiddleware(),
		loggerInjector,
		JWTVerifier,
	}

	// Mount the Native API /events endpoint with all default middlewares, adding the authentication middlewares
	if conf.Server.DevActivityPanel {
		r.Group(func(r chi.Router) {
			r.Use(defaultMiddlewares...)
			r.Use(Authenticator(s.ds))
			r.Use(JWTRefresher)
			r.Handle(path.Join(conf.Server.BasePath, consts.URLPathNativeAPI, "events"), s.broker)
		})
	}

	// Configure the router with the default middlewares and requestLogger
	r.Group(func(r chi.Router) {
		r.Use(defaultMiddlewares...)
		r.Use(requestLogger)
		s.router = r
	})
}

func (s *Server) mountAuthenticationRoutes() chi.Router {
	r := s.router
	return r.Route(path.Join(conf.Server.BasePath, "/auth"), func(r chi.Router) {
		r.Use(LimitLoginBody)
		if conf.Server.AuthRequestLimit > 0 {
			log.Info("Login rate limit set", "requestLimit", conf.Server.AuthRequestLimit,
				"windowLength", conf.Server.AuthWindowLength)

			rateLimiter := ClientIPRateLimiter(conf.Server.AuthRequestLimit, conf.Server.AuthWindowLength)
			r.With(rateLimiter).Post("/login", login(s.ds))
		} else {
			log.Warn("Login rate limit is disabled! Consider enabling it to be protected against brute-force attacks")

			r.Post("/login", login(s.ds))
		}
		r.Post("/createAdmin", createAdmin(s.ds))
	})
}

// Serve UI app assets
func (s *Server) mountRootRedirector() {
	r := s.router
	// Redirect root to UI URL. This is an all-method wildcard because a few legacy
	// clients send their API calls without /jellyfin or /emby; dispatchRootCompat
	// gets first refusal for the explicitly supported API prefixes. Non-GET requests
	// that do not match an alias retain the old wildcard's 405 response.
	r.Handle("/*", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.dispatchRootCompat(w, r) {
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			http.Redirect(w, r, s.appRoot+"/", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	r.Get(s.appRoot, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.appRoot+"/", http.StatusFound)
	})
}

// dispatchRootCompat serves a request through a registered root alias. It returns
// false when the request is outside BasePath or does not begin with one of the
// explicitly registered prefixes.
func (s *Server) dispatchRootCompat(w http.ResponseWriter, r *http.Request) bool {
	relative, ok := stripBasePath(r.URL.Path, conf.Server.BasePath)
	if !ok {
		return false
	}

	for _, route := range s.rootCompatRoutes {
		if !matchesRootCompatPrefix(relative, route.prefixes) {
			continue
		}

		// The parent router has already populated chi.RouteCtxKey for its wildcard
		// route. Store a typed nil there so the child chi mux creates a fresh context
		// instead of reusing the parent's wildcard parameters and route path.
		routed := r.Clone(context.WithValue(r.Context(), chi.RouteCtxKey, (*chi.Context)(nil)))
		u := *r.URL
		u.Path = relative
		u.RawPath = ""
		routed.URL = &u
		routed.RequestURI = relative
		if u.RawQuery != "" {
			routed.RequestURI += "?" + u.RawQuery
		}
		routed.Pattern = ""
		route.handler.ServeHTTP(w, routed)
		return true
	}

	return false
}

func normalizeRootCompatPrefixes(prefixes []string) []string {
	result := make([]string, 0, len(prefixes))
	seen := make(map[string]struct{}, len(prefixes))
	for _, prefix := range prefixes {
		prefix = strings.TrimSpace(prefix)
		prefix = strings.Trim(prefix, "/")
		if prefix == "" {
			continue
		}
		prefix = strings.ToLower(prefix)
		if _, exists := seen[prefix]; exists {
			continue
		}
		seen[prefix] = struct{}{}
		result = append(result, prefix)
	}
	return result
}

func matchesRootCompatPrefix(relative string, prefixes []string) bool {
	relative = strings.ToLower(relative)
	for _, prefix := range prefixes {
		prefix = strings.ToLower(prefix)
		if relative == "/"+prefix || strings.HasPrefix(relative, "/"+prefix+"/") {
			return true
		}
	}
	return false
}

// stripBasePath removes the configured BasePath from a request path while
// preserving a leading slash for the child router. It requires a segment boundary
// so /musicbox cannot accidentally match a configured /music base path.
func stripBasePath(requestPath, basePath string) (string, bool) {
	if requestPath == "" {
		requestPath = "/"
	}
	basePath = strings.Trim(strings.TrimSpace(basePath), "/")
	if basePath == "" {
		return requestPath, strings.HasPrefix(requestPath, "/")
	}
	basePath = "/" + basePath
	if requestPath == basePath {
		return "/", true
	}
	if !strings.HasPrefix(requestPath, basePath+"/") {
		return "", false
	}
	relative := strings.TrimPrefix(requestPath, basePath)
	if relative == "" {
		relative = "/"
	}
	return relative, true
}

func (s *Server) frontendAssetsHandler() http.Handler {
	r := chi.NewRouter()

	r.Handle("/", Index(s.ds, ui.BuildAssets()))
	r.Handle("/*", http.StripPrefix(s.appRoot, http.FileServer(http.FS(ui.BuildAssets()))))
	return r
}

// validateTLSCertificates validates the TLS certificate and key files before starting the server.
// It provides detailed error messages for common issues like encrypted private keys.
func validateTLSCertificates(certFile, keyFile string) error {
	// Read the key file to check for encryption
	keyData, err := os.ReadFile(keyFile) //nolint:gosec
	if err != nil {
		return fmt.Errorf("reading TLS key file: %w", err)
	}

	// Parse PEM blocks and check for encryption
	block, _ := pem.Decode(keyData)
	if block == nil {
		return errors.New("TLS key file does not contain a valid PEM block")
	}

	// Check for encrypted private key indicators
	if isEncryptedPEM(block, keyData) {
		return errors.New("TLS private key is encrypted (password-protected). " +
			"Navidrome does not support encrypted private keys. " +
			"Please decrypt your key using: openssl pkey -in <encrypted-key> -out <decrypted-key>")
	}

	// Try to load the certificate pair to validate it
	_, err = tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("loading TLS certificate/key pair: %w", err)
	}

	return nil
}

// isEncryptedPEM checks if a PEM block represents an encrypted private key.
func isEncryptedPEM(block *pem.Block, rawData []byte) bool {
	// Check for PKCS#8 encrypted format (BEGIN ENCRYPTED PRIVATE KEY)
	if block.Type == "ENCRYPTED PRIVATE KEY" {
		return true
	}

	// Check for legacy encrypted format with Proc-Type header
	if block.Headers != nil {
		if procType, ok := block.Headers["Proc-Type"]; ok && strings.Contains(procType, "ENCRYPTED") {
			return true
		}
	}

	// Also check raw data for DEK-Info header (in case pem.Decode doesn't parse headers correctly)
	if bytes.Contains(rawData, []byte("DEK-Info:")) || bytes.Contains(rawData, []byte("Proc-Type: 4,ENCRYPTED")) {
		return true
	}

	return false
}
