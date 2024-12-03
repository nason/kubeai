package messenger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/substratusai/kubeai/internal/apiutils"
	"github.com/substratusai/kubeai/internal/loadbalancer"
	"github.com/substratusai/kubeai/internal/metrics"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"gocloud.dev/pubsub"
)

type Messenger struct {
	modelScaler  ModelScaler
	loadBalancer LoadBalancer

	HTTPC *http.Client

	MaxHandlers     int
	ErrorMaxBackoff time.Duration

	requestsURL string
	requests    *pubsub.Subscription
	responses   *pubsub.Topic

	consecutiveErrorsMtx sync.RWMutex
	consecutiveErrors    int
}

func NewMessenger(
	ctx context.Context,
	requestsURL string,
	responsesURL string,
	maxHandlers int,
	errorMaxBackoff time.Duration,
	modelScaler ModelScaler,
	lb LoadBalancer,
	httpClient *http.Client,
) (*Messenger, error) {
	requests, err := pubsub.OpenSubscription(ctx, requestsURL)
	if err != nil {
		return nil, err
	}

	responses, err := pubsub.OpenTopic(ctx, responsesURL)
	if err != nil {
		return nil, err
	}

	return &Messenger{
		modelScaler:     modelScaler,
		loadBalancer:    lb,
		HTTPC:           httpClient,
		requestsURL:     requestsURL,
		requests:        requests,
		responses:       responses,
		MaxHandlers:     maxHandlers,
		ErrorMaxBackoff: errorMaxBackoff,
	}, nil
}

type ModelScaler interface {
	LookupModel(ctx context.Context, model, adapter string, selectors []string) (bool, error)
	ScaleAtLeastOneReplica(ctx context.Context, model string) error
}

type LoadBalancer interface {
	AwaitBestAddress(ctx context.Context, req loadbalancer.AddressRequest) (string, func(), error)
}

func (m *Messenger) Start(ctx context.Context) error {
	sem := make(chan struct{}, m.MaxHandlers)

	var restartAttempt int
	const maxRestartAttempts = 20
	const maxRestartBackoff = 10 * time.Second

	log.Printf("Messenger starting receive loop for requests subscription %q", m.requestsURL)
recvLoop:
	for {
		msg, err := m.requests.Receive(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}

			if restartAttempt > maxRestartAttempts {
				log.Printf("Error receiving message: %v. Restarted subscription %d times, giving up.",
					err, restartAttempt)
				return err
			}

			// If there is a non-recoverable error, recreate the
			// subscription and continue receiving messages.
			// This is important so existing handlers can continue.
			log.Printf("Error receiving message: %v", err)
			// Shutdown isn't strictly necessary, but it's good practice.
			shutdownErr := m.requests.Shutdown(ctx)
			if shutdownErr != nil {
				log.Printf("Error shutting down requests topic: %v. Continuing to recreate subscription.",
					shutdownErr)
			}
			restartWait := min(time.Duration(restartAttempt)*time.Second, maxRestartBackoff)
			log.Printf("Waiting %v before recreating requests subscription %v", restartWait, m.requestsURL)
			time.Sleep(restartWait)

			var subErr error
			m.requests, subErr = pubsub.OpenSubscription(ctx, m.requestsURL)
			if subErr != nil {
				log.Printf("Error recreating requests subscription %v: %v",
					m.requestsURL, subErr)
				return subErr
			}

			restartAttempt++
			continue
		} else {
			restartAttempt = 0
		}

		log.Println("Received message:", msg.LoggableID)

		// Wait if there are too many active handle goroutines and acquire the
		// semaphore. If the context is canceled, stop waiting and start shutting
		// down.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break recvLoop
		}

		go func() {
			defer func() { <-sem }()
			m.handleRequest(context.Background(), msg)
		}()

		// Slow down a bit to avoid churning through messages and running
		// up cloud costs PubSub & GPUs when no meaningful work is being done.
		//
		// Intended to mitigate cases such as:
		// * Spontaneous failures that might creep up overnight.
		//   (Slow and speed back up later)
		// * Some request-generation job sending a million malformed requests into a topic.
		//   (Slow until an admin can intervene)
		if consecutiveErrors := m.getConsecutiveErrors(); consecutiveErrors > 0 {
			wait := consecutiveErrBackoff(consecutiveErrors, m.ErrorMaxBackoff)
			log.Printf("after %d consecutive errors, waiting %v before processing next message", consecutiveErrors, wait)
			time.Sleep(wait)
		}
	}

	// We're no longer receiving messages. Wait to finish handling any
	// unacknowledged messages by totally acquiring the semaphore.
	for n := 0; n < m.MaxHandlers; n++ {
		sem <- struct{}{}
	}

	return nil
}

func consecutiveErrBackoff(n int, max time.Duration) time.Duration {
	d := time.Duration(n) * time.Second
	if d > max {
		return max
	}
	return d
}

func (m *Messenger) handleRequest(ctx context.Context, msg *pubsub.Message) {
	// Expecting a message with the following structure:
	/*
		{
			"metadata": {
				"some-sort-of-id": 123,
				"optional-key": "some-user-value"
				# ...
			},
			"path": "/v1/completions",
			"body": {
				"model": "test-model"
				# ... other OpenAI compatible fields
			}
		}
	*/
	req, err := parseRequest(ctx, msg)
	if err != nil {
		m.sendResponse(req, m.jsonError("error parsing request: %v", err), http.StatusBadRequest)
		return
	}

	metricAttrs := metric.WithAttributeSet(attribute.NewSet(
		metrics.AttrRequestModel.String(req.model),
		metrics.AttrRequestType.String(metrics.AttrRequestTypeMessage),
	))
	metrics.InferenceRequestsActive.Add(ctx, 1, metricAttrs)
	defer metrics.InferenceRequestsActive.Add(ctx, -1, metricAttrs)

	modelExists, err := m.modelScaler.LookupModel(ctx, req.model, req.adapter, nil)
	if err != nil {
		m.sendResponse(req, m.jsonError("error checking if model exists: %v", err), http.StatusInternalServerError)
		return
	}
	if !modelExists {
		// Send a 400 response to the client, however it is possible the backend
		// will be deployed soon or another subscriber will handle it.
		m.sendResponse(req, m.jsonError("model not found: %s", req.model), http.StatusNotFound)
		return
	}

	// Ensure the backend is scaled to at least one Pod.
	m.modelScaler.ScaleAtLeastOneReplica(ctx, req.model)

	log.Printf("Awaiting host for message %s", msg.LoggableID)

	host, completeFunc, err := m.loadBalancer.AwaitBestAddress(ctx, loadbalancer.AddressRequest{
		Model:   req.model,
		Adapter: req.adapter,
		// TODO: Prefix
	})
	if err != nil {
		m.sendResponse(req, m.jsonError("error awaiting host for backend: %v", err), http.StatusBadGateway)
		return
	}
	defer completeFunc()

	url := fmt.Sprintf("http://%s%s", host, req.path)
	log.Printf("Sending request to backend for message %s: %s", msg.LoggableID, url)
	respPayload, respCode, err := m.sendBackendRequest(ctx, url, req.body)
	if err != nil {
		m.sendResponse(req, m.jsonError("error sending request to backend: %v", err), http.StatusBadGateway)
		return
	}

	m.sendResponse(req, respPayload, respCode)
}

func (m *Messenger) Stop(ctx context.Context) error {
	return m.requests.Shutdown(ctx)
}

type request struct {
	ctx            context.Context
	msg            *pubsub.Message
	metadata       map[string]interface{}
	path           string
	body           json.RawMessage
	requestedModel string
	model          string
	adapter        string
}

func parseRequest(ctx context.Context, msg *pubsub.Message) (*request, error) {
	req := &request{
		ctx: ctx,
		msg: msg,
	}

	var payload struct {
		Metadata map[string]interface{} `json:"metadata"`
		Path     string                 `json:"path"`
		Body     json.RawMessage        `json:"body"`
	}
	if err := json.Unmarshal(msg.Body, &payload); err != nil {
		return req, fmt.Errorf("unmarshalling message as json: %w", err)
	}

	path := payload.Path
	if payload.Path == "" {
		// Default to completions endpoint.
		path = "/v1/completions"
	} else if !strings.HasPrefix(payload.Path, "/") {
		path = "/" + payload.Path
	}

	req.metadata = payload.Metadata
	req.path = path
	req.body = payload.Body

	var payloadBody map[string]interface{}
	if err := json.Unmarshal(payload.Body, &payloadBody); err != nil {
		return nil, fmt.Errorf("decoding: %w", err)
	}
	modelInf, ok := payloadBody["model"]
	if !ok {
		return nil, fmt.Errorf("missing '.body.model' field")
	}
	modelStr, ok := modelInf.(string)
	if !ok {
		return nil, fmt.Errorf("field '.body.model' should be a string")
	}

	req.requestedModel = modelStr
	req.model, req.adapter = apiutils.SplitModelAdapter(modelStr)

	// Assuming this is a vLLM request.
	// vLLM expects the adapter to be in the model field.
	if req.adapter != "" {
		payloadBody["model"] = req.adapter
		rewrittenBody, err := json.Marshal(payloadBody)
		if err != nil {
			return nil, fmt.Errorf("remarshalling: %w", err)
		}
		req.body = rewrittenBody
	}

	return req, nil
}

func (m *Messenger) sendBackendRequest(ctx context.Context, url string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := m.HTTPC.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}

	return payload, resp.StatusCode, nil
}

func (m *Messenger) sendResponse(req *request, body []byte, statusCode int) {
	log.Printf("Sending response to message: %v", req.msg.LoggableID)

	response := struct {
		Metadata   map[string]interface{} `json:"metadata"`
		StatusCode int                    `json:"status_code"`
		Body       json.RawMessage        `json:"body"`
	}{
		Metadata:   req.metadata,
		StatusCode: statusCode,
		Body:       body,
	}

	jsonResponse, err := json.Marshal(response)
	if err != nil {
		log.Println("Error marshalling response:", err)
		m.addConsecutiveError()
	}

	if err := m.responses.Send(req.ctx, &pubsub.Message{
		Body: jsonResponse,
		Metadata: map[string]string{
			"request_message_id": req.msg.LoggableID,
		},
	}); err != nil {
		log.Printf("Error sending response for message %s: %v", req.msg.LoggableID, err)
		m.addConsecutiveError()

		// If a response cant be sent, the message should be redelivered.
		if req.msg.Nackable() {
			req.msg.Nack()
		}
		return
	}

	log.Printf("Send response for message: %s", req.msg.LoggableID)
	if statusCode < 300 {
		m.resetConsecutiveErrors()
	}
	req.msg.Ack()
}

func (m *Messenger) jsonError(format string, args ...interface{}) []byte {
	m.addConsecutiveError()

	message := fmt.Sprintf(format, args...)
	log.Println(message)

	// Example OpenAI error response:
	/*
		{
		  "error": {
		    "message": "Invalid authorization header",
		    "type": "server_error",
		    "param": null,
		    "code": null
		  }
	*/
	return []byte(fmt.Sprintf(`{
	"error": {
		"message": %q
	}
}`, message))
}

func (m *Messenger) addConsecutiveError() {
	m.consecutiveErrorsMtx.Lock()
	defer m.consecutiveErrorsMtx.Unlock()
	m.consecutiveErrors++
}

func (m *Messenger) resetConsecutiveErrors() {
	m.consecutiveErrorsMtx.Lock()
	defer m.consecutiveErrorsMtx.Unlock()
	m.consecutiveErrors = 0
}

func (m *Messenger) getConsecutiveErrors() int {
	m.consecutiveErrorsMtx.RLock()
	defer m.consecutiveErrorsMtx.RUnlock()
	return m.consecutiveErrors
}
