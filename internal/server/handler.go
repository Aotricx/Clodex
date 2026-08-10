package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"

	"github.com/Aotricx/Clodex/internal/anthropic"
	"github.com/Aotricx/Clodex/internal/catalog"
	"github.com/Aotricx/Clodex/internal/codexwire"
	"github.com/Aotricx/Clodex/internal/redact"
	"github.com/Aotricx/Clodex/internal/requestprep"
	clodexstatus "github.com/Aotricx/Clodex/internal/status"
)

// CatalogResolver is implemented by catalog.Manager.
type CatalogResolver interface {
	Resolve(context.Context) (catalog.Resolution, error)
}

// RequestCounter is implemented by tokenizer.Counter.
type RequestCounter interface {
	CountRequest(codexwire.Request) (int, error)
}

// Options contains required HTTP-boundary dependencies.
type Options struct {
	Version      string
	Status       *clodexstatus.State
	Catalog      CatalogResolver
	Counter      RequestCounter
	DefaultModel string
	Messages     http.Handler
	BeforeStatus func(context.Context) error
}

// Handler serves the Anthropic-compatible loopback surface.
type Handler struct {
	version      string
	status       *clodexstatus.State
	catalog      CatalogResolver
	counter      RequestCounter
	defaultModel string
	messages     http.Handler
	beforeStatus func(context.Context) error
}

// New validates every shipping dependency before exposing an HTTP handler.
func New(options Options) (*Handler, error) {
	if strings.TrimSpace(options.Version) == "" {
		return nil, errors.New("create Clodex handler: version is required")
	}
	if options.Status == nil {
		return nil, errors.New("create Clodex handler: status state is required")
	}
	if options.Catalog == nil {
		return nil, errors.New("create Clodex handler: catalog resolver is required")
	}
	if options.Counter == nil {
		return nil, errors.New("create Clodex handler: tokenizer is required")
	}
	if strings.TrimSpace(options.DefaultModel) == "" {
		return nil, errors.New("create Clodex handler: default model is required")
	}
	if options.Messages == nil {
		return nil, errors.New("create Clodex handler: messages handler is required")
	}
	options.Status.SetVersion(options.Version)
	return &Handler{
		version: options.Version, status: options.Status, catalog: options.Catalog,
		counter: options.Counter, defaultModel: options.DefaultModel, messages: options.Messages, beforeStatus: options.BeforeStatus,
	}, nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Clodex-Version", handler.version)
	writer.Header().Set("Cache-Control", "no-store")

	switch request.URL.Path {
	case "/":
		if request.Method != http.MethodHead {
			handler.writeNotFound(writer)
			return
		}
		writer.WriteHeader(http.StatusOK)
	case "/healthz":
		if !allowMethod(writer, request, http.MethodGet, http.MethodHead) {
			return
		}
		handler.health(writer, request.Method == http.MethodHead)
	case "/status":
		if !allowMethod(writer, request, http.MethodGet, http.MethodHead) {
			return
		}
		if handler.beforeStatus != nil {
			if err := handler.beforeStatus(request.Context()); err != nil {
				handler.writeError(writer, http.StatusServiceUnavailable, "api_error", redact.Text(err.Error()))
				return
			}
		}
		handler.writeJSON(writer, http.StatusOK, handler.status.Snapshot(), request.Method == http.MethodHead)
	case "/v1/models":
		if !allowMethod(writer, request, http.MethodGet, http.MethodHead) {
			return
		}
		handler.models(writer, request)
	case "/v1/messages/count_tokens":
		if !allowMethod(writer, request, http.MethodPost) {
			return
		}
		if !requireJSON(writer, request) {
			return
		}
		handler.countTokens(writer, request)
	case "/v1/messages":
		if !allowMethod(writer, request, http.MethodPost) {
			return
		}
		if !requireJSON(writer, request) {
			return
		}
		end := handler.status.BeginSession()
		defer end()
		handler.messages.ServeHTTP(writer, request)
	default:
		handler.writeNotFound(writer)
	}
}

func (handler *Handler) health(writer http.ResponseWriter, head bool) {
	handler.writeJSON(writer, http.StatusOK, map[string]string{
		"status": "ok", "service": "clodex", "version": handler.version,
	}, head)
}

func (handler *Handler) models(writer http.ResponseWriter, request *http.Request) {
	resolution, err := handler.resolveCatalog(request.Context())
	if err != nil {
		handler.writeError(writer, http.StatusServiceUnavailable, "api_error", redact.Text(err.Error()))
		return
	}
	list, err := ListModels(resolution.Catalog, request.URL.Query())
	if err != nil {
		handler.writeAnyError(writer, err)
		return
	}
	handler.writeJSON(writer, http.StatusOK, list, request.Method == http.MethodHead)
}

func (handler *Handler) countTokens(writer http.ResponseWriter, request *http.Request) {
	decoded, err := anthropic.DecodeCountTokensRequest(request.Body)
	if err != nil {
		handler.writeAnyError(writer, err)
		return
	}
	resolution, err := handler.resolveCatalog(request.Context())
	if err != nil {
		handler.writeError(writer, http.StatusServiceUnavailable, "api_error", redact.Text(err.Error()))
		return
	}
	prepared, err := requestprep.Prepare(resolution.Catalog, decoded, handler.defaultModel, request.Header.Get("x-claude-code-session-id"))
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request_error", redact.Text(err.Error()))
		return
	}
	for _, warning := range prepared.Translation.Warnings {
		handler.status.RecordWarnings(warning.Kind, int64(warning.Count))
	}
	count, err := handler.counter.CountRequest(prepared.Translation.Request)
	if err != nil {
		handler.writeError(writer, http.StatusInternalServerError, "api_error", redact.Text(err.Error()))
		return
	}
	handler.writeJSON(writer, http.StatusOK, struct {
		InputTokens int `json:"input_tokens"`
	}{InputTokens: count}, false)
}

func (handler *Handler) resolveCatalog(ctx context.Context) (catalog.Resolution, error) {
	resolution, err := handler.catalog.Resolve(ctx)
	if err != nil {
		return catalog.Resolution{}, err
	}
	handler.status.SetCatalog(string(resolution.Source), resolution.Catalog.FetchedAt)
	return resolution, nil
}

func allowMethod(writer http.ResponseWriter, request *http.Request, methods ...string) bool {
	for _, method := range methods {
		if request.Method == method {
			return true
		}
	}
	writer.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(writer, http.StatusMethodNotAllowed, "invalid_request_error", fmt.Sprintf("method %s is not allowed for %s", request.Method, request.URL.Path))
	return false
}

func requireJSON(writer http.ResponseWriter, request *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		writeError(writer, http.StatusBadRequest, "invalid_request_error", "Content-Type must be application/json")
		return false
	}
	return true
}

func (handler *Handler) writeNotFound(writer http.ResponseWriter) {
	handler.writeError(writer, http.StatusNotFound, "not_found_error", "requested Clodex endpoint was not found")
}

func (handler *Handler) writeAnyError(writer http.ResponseWriter, err error) {
	var requestError *anthropic.RequestError
	if errors.As(err, &requestError) {
		handler.writeJSON(writer, requestError.StatusCode, requestError.Response, false)
		return
	}
	handler.writeError(writer, http.StatusInternalServerError, "api_error", redact.Text(err.Error()))
}

func (handler *Handler) writeError(writer http.ResponseWriter, statusCode int, errorType, message string) {
	writeError(writer, statusCode, errorType, message)
}

func writeError(writer http.ResponseWriter, statusCode int, errorType, message string) {
	writeJSON(writer, statusCode, anthropic.ErrorResponse{
		Type: "error", Error: anthropic.ErrorDetail{Type: errorType, Message: message},
	}, false)
}

func (handler *Handler) writeJSON(writer http.ResponseWriter, statusCode int, value any, head bool) {
	writeJSON(writer, statusCode, value, head)
}

func writeJSON(writer http.ResponseWriter, statusCode int, value any, head bool) {
	encoded, err := json.Marshal(value)
	if err != nil {
		encoded = []byte(`{"type":"error","error":{"type":"api_error","message":"encode Clodex response"}}`)
		statusCode = http.StatusInternalServerError
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(statusCode)
	if head {
		return
	}
	_, _ = writer.Write(encoded)
}
