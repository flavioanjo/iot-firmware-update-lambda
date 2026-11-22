package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// lambdaFunc is the signature shared by HandleRequest and firmwareHandler.Handle.
type lambdaFunc func(context.Context, events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error)

// Local download routes. The first one is canonical; the others are aliases
// kept because earlier documentation and the device simulator used them.
var localDownloadRoutes = []string{
	"/default/DownloadIOTFirmware",
	"/default/download_firmware",
	"/default/DownloadFirmware",
}

var localHealthRoutes = []string{"/health", "/default/health"}

// newLocalMux exposes a Lambda handler over plain HTTP.
func newLocalMux(fn lambdaFunc, logger *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	for _, route := range localHealthRoutes {
		mux.Handle(route, lambdaAdapter(fn, logger, true))
	}
	for _, route := range localDownloadRoutes {
		mux.Handle(route, lambdaAdapter(fn, logger, false))
	}
	return mux
}

// lambdaAdapter converts an HTTP request into an API Gateway proxy event,
// invokes fn with the request context, and writes the proxy response back.
//
// forwardPath controls whether r.URL.Path is copied into the event. It is set
// for the health routes only: the handler treats "/default/DownloadIOTFirmware"
// as a health path (for load balancer compatibility), so forwarding it on the
// download routes would turn every local download into a health check. Leaving
// it empty matches an API Gateway REST integration, where Path is the resource
// path without the stage prefix.
func lambdaAdapter(fn lambdaFunc, logger *slog.Logger, forwardPath bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		query := r.URL.Query()
		event := events.APIGatewayProxyRequest{
			HTTPMethod:                      r.Method,
			Headers:                         map[string]string{},
			MultiValueHeaders:               map[string][]string(r.Header.Clone()),
			QueryStringParameters:           map[string]string{},
			MultiValueQueryStringParameters: map[string][]string(query),
		}
		if forwardPath {
			event.Path = r.URL.Path
		}
		for key, values := range query {
			// API Gateway REST keeps the last value of a repeated parameter.
			event.QueryStringParameters[key] = values[len(values)-1]
		}
		for key := range r.Header {
			event.Headers[key] = r.Header.Get(key)
		}

		resp, err := fn(r.Context(), event)
		if err != nil {
			logger.Error("handler returned an error", slog.String("error", err.Error()))
			http.Error(w, "Internal server error.", http.StatusInternalServerError)
			return
		}

		body := []byte(resp.Body)
		if resp.IsBase64Encoded {
			body, err = base64.StdEncoding.DecodeString(resp.Body)
			if err != nil {
				logger.Error("handler returned invalid base64 body", slog.String("error", err.Error()))
				http.Error(w, "Internal server error.", http.StatusInternalServerError)
				return
			}
		}

		for key, value := range resp.Headers {
			w.Header().Set(key, value)
		}
		for key, values := range resp.MultiValueHeaders {
			for _, v := range values {
				w.Header().Add(key, v)
			}
		}
		// Always derive Content-Length from the bytes actually written.
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		status := resp.StatusCode
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if r.Method == http.MethodHead {
			return
		}
		if _, err := w.Write(body); err != nil {
			logger.Warn("writing response failed", slog.String("error", err.Error()))
		}
	})
}

// runLocalServer serves HandleRequest on PORT (default 8080) until Ctrl+C.
func runLocalServer() error {
	logger := newLogger()
	cfg := loadConfig(logger)

	srv := &http.Server{
		Addr:              net.JoinHostPort("", cfg.LocalPort),
		Handler:           newLocalMux(HandleRequest, logger),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	fmt.Println("==================================================")
	fmt.Printf("   Firmware server running on port %s\n", cfg.LocalPort)
	fmt.Printf("   Download: http://localhost:%s%s\n", cfg.LocalPort, localDownloadRoutes[0])
	fmt.Printf("   Health:   http://localhost:%s/default/health\n", cfg.LocalPort)
	fmt.Println("   Press Ctrl+C to stop.")
	fmt.Println("==================================================")

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
