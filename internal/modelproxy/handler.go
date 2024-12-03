package modelproxy

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/substratusai/kubeai/internal/loadbalancer"
	"github.com/substratusai/kubeai/internal/metrics"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type ModelClient interface {
	LookupModel(ctx context.Context, model, adapter string, selectors []string) (bool, error)
	ScaleAtLeastOneReplica(ctx context.Context, model string) error
}

type LoadBalancer interface {
	AwaitBestAddress(ctx context.Context, req loadbalancer.AddressRequest) (string, func(), error)
}

// Handler serves http requests for end-clients.
// It is also responsible for triggering scale-from-zero.
type Handler struct {
	modelScaler  ModelClient
	loadBalancer LoadBalancer
	maxRetries   int
	retryCodes   map[int]struct{}
}

func NewHandler(
	modelScaler ModelClient,
	loadBalancer LoadBalancer,
	maxRetries int,
	retryCodes map[int]struct{},
) *Handler {
	return &Handler{
		modelScaler:  modelScaler,
		loadBalancer: loadBalancer,
		maxRetries:   maxRetries,
		retryCodes:   retryCodes,
	}
}

var defaultRetryCodes = map[int]struct{}{
	http.StatusInternalServerError: {},
	http.StatusBadGateway:          {},
	http.StatusServiceUnavailable:  {},
	http.StatusGatewayTimeout:      {},
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("url: %v", r.URL)

	w.Header().Set("X-Proxy", "lingo")

	pr := newProxyRequest(r)

	// TODO: Only parse model for paths that would have a model.
	if err := pr.parse(); err != nil {
		pr.sendErrorResponse(w, http.StatusBadRequest, "unable to parse model: %v", err)
		return
	}

	log.Println("model:", pr.model, "adapter:", pr.adapter)

	metricAttrs := metric.WithAttributeSet(attribute.NewSet(
		metrics.AttrRequestModel.String(pr.requestedModel),
		metrics.AttrRequestType.String(metrics.AttrRequestTypeHTTP),
	))
	metrics.InferenceRequestsActive.Add(pr.r.Context(), 1, metricAttrs)
	defer metrics.InferenceRequestsActive.Add(pr.r.Context(), -1, metricAttrs)

	modelExists, err := h.modelScaler.LookupModel(r.Context(), pr.model, pr.adapter, pr.selectors)
	if err != nil {
		pr.sendErrorResponse(w, http.StatusInternalServerError, "unable to resolve model: %v", err)
		return
	}
	if !modelExists {
		pr.sendErrorResponse(w, http.StatusNotFound, "model not found: %v", pr.requestedModel)
		return
	}

	// Ensure the backend is scaled to at least one Pod.
	if err := h.modelScaler.ScaleAtLeastOneReplica(r.Context(), pr.model); err != nil {
		pr.sendErrorResponse(w, http.StatusInternalServerError, "unable to scale model: %v", err)
		return
	}

	h.proxyHTTP(w, pr)
}

// AdditionalProxyRewrite is an injection point for modifying proxy requests.
// Used in tests.
var AdditionalProxyRewrite = func(*httputil.ProxyRequest) {}

func (h *Handler) proxyHTTP(w http.ResponseWriter, pr *proxyRequest) {
	log.Printf("Waiting for host: %v", pr.id)

	addr, decrementInflight, err := h.loadBalancer.AwaitBestAddress(pr.r.Context(), loadbalancer.AddressRequest{
		Model:   pr.model,
		Adapter: pr.adapter,
		// TODO: Prefix
	})
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			pr.sendErrorResponse(w, http.StatusInternalServerError, "request cancelled while finding host: %v", err)
			return
		case errors.Is(err, context.DeadlineExceeded):
			pr.sendErrorResponse(w, http.StatusGatewayTimeout, "request timeout while finding host: %v", err)
			return
		default:
			pr.sendErrorResponse(w, http.StatusGatewayTimeout, "unable to find host: %v", err)
			return
		}
	}
	// NOTE: decrementInflight will be called after the request succeeds or fails after all retries.
	defer decrementInflight()

	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&url.URL{
				Scheme: "http",
				Host:   addr,
			})
			r.Out.Host = r.In.Host
			AdditionalProxyRewrite(r)
		},
	}

	proxy.ModifyResponse = func(r *http.Response) error {
		// Record the response for metrics.
		pr.status = r.StatusCode

		// This point is reached if a response code is received.
		if h.isRetryCode(r.StatusCode) && pr.attempt < h.maxRetries {
			// Returning an error will trigger the ErrorHandler.
			return ErrRetry
		}

		return nil
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// This point could be reached if a bad response code was sent by the backend
		// or
		// if there was an issue with the connection and no response was ever received.
		if err != nil && r.Context().Err() == nil && pr.attempt < h.maxRetries {
			pr.attempt++

			log.Printf("Retrying request (%v/%v): %v: %v", pr.attempt, h.maxRetries, pr.id, err)
			h.proxyHTTP(w, pr)
			return
		}

		if !errors.Is(err, ErrRetry) {
			pr.sendErrorResponse(w, http.StatusBadGateway, "proxy: exceeded retries: %v/%v", pr.attempt, h.maxRetries)
		}
	}

	log.Printf("Proxying request to ip %v: %v\n", addr, pr.id)
	proxy.ServeHTTP(w, pr.httpRequest())
}

var ErrRetry = errors.New("retry")

func (h *Handler) isRetryCode(status int) bool {
	var retry bool
	// TODO: avoid the nil check here and set a default map in the constructor.
	if h.retryCodes != nil {
		_, retry = h.retryCodes[status]
	} else {
		_, retry = defaultRetryCodes[status]
	}
	return retry
}
