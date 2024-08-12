package webmetric

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	"k8s.io/client-go/util/jsonpath"

	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	"github.com/argoproj/argo-rollouts/utils/evaluate"
	metricutil "github.com/argoproj/argo-rollouts/utils/metric"
	timeutil "github.com/argoproj/argo-rollouts/utils/time"
)

const (
	// ProviderType indicates the provider is a web metric
	ProviderType         = "Web"
	ContentTypeKey       = "Content-Type"
	ContentTypeJsonValue = "application/json"
)

// Provider contains all the required components to run a WebMetric query
// Implements the Provider Interface
type Provider struct {
	logCtx     log.Entry
	client     *http.Client
	jsonParser *jsonpath.JSONPath
}

// Type indicates provider is a WebMetric provider
func (p *Provider) Type() string {
	return ProviderType
}

// GetMetadata returns any additional metadata which needs to be stored & displayed as part of the metrics result.
func (p *Provider) GetMetadata(metric v1alpha1.Metric) map[string]string {
	return nil
}

func (p *Provider) Run(run *v1alpha1.AnalysisRun, metric v1alpha1.Metric) v1alpha1.Measurement {
	startTime := timeutil.MetaNow()

	// Measurement to pass back
	measurement := v1alpha1.Measurement{
		StartedAt: &startTime,
	}

	method := v1alpha1.WebMetricMethodGet
	if metric.Provider.Web.Method != "" {
		method = metric.Provider.Web.Method
	}

	url := metric.Provider.Web.URL

	stringBody := metric.Provider.Web.Body
	jsonBody := metric.Provider.Web.JSONBody

	var body io.Reader

	if stringBody != "" && jsonBody != nil {
		return metricutil.MarkMeasurementError(measurement, fmt.Errorf("use either Body or JSONBody; both cannot exists for WebMetric payload"))
	} else if (stringBody != "" || jsonBody != nil) && method == v1alpha1.WebMetricMethodGet {
		return metricutil.MarkMeasurementError(measurement, fmt.Errorf("Body/JSONBody can only be used with POST or PUT WebMetric Method types"))
	}

	if stringBody != "" {
		body = strings.NewReader(stringBody)
	} else if jsonBody != nil {
		bodyBytes, err := jsonBody.MarshalJSON()
		if err != nil {
			return metricutil.MarkMeasurementError(measurement, err)
		}
		body = bytes.NewBuffer(bodyBytes)
	}

	// Create request
	request, err := http.NewRequest(string(method), url, body)
	if err != nil {
		return metricutil.MarkMeasurementError(measurement, err)
	}

	request.Header = make(http.Header)

	for _, header := range metric.Provider.Web.Headers {
		request.Header.Set(header.Key, header.Value)
	}
	if jsonBody != nil {
		request.Header.Set(ContentTypeKey, ContentTypeJsonValue)
	}

	// Send Request
	response, err := p.client.Do(request)
	if err != nil {
		return metricutil.MarkMeasurementError(measurement, err)
	} else if response.StatusCode < 200 || response.StatusCode >= 300 {
		return metricutil.MarkMeasurementError(measurement, fmt.Errorf("received non 2xx response code: %v", response.StatusCode))
	}

	value, status, err := p.parseResponse(metric, response)
	if err != nil {
		return metricutil.MarkMeasurementError(measurement, err)
	}

	measurement.Value = value
	measurement.Phase = status
	finishedTime := timeutil.MetaNow()
	measurement.FinishedAt = &finishedTime

	return measurement
}

func (p *Provider) parseResponse(metric v1alpha1.Metric, response *http.Response) (string, v1alpha1.AnalysisPhase, error) {
	var data any

	bodyBytes, err := io.ReadAll(response.Body)
	if err != nil {
		return "", v1alpha1.AnalysisPhaseError, fmt.Errorf("Received no bytes in response: %v", err)
	}

	err = json.Unmarshal(bodyBytes, &data)
	if err != nil {
		// non JSON body return as string
		return string(bodyBytes), v1alpha1.AnalysisPhaseSuccessful, nil
	}

	fullResults, err := p.jsonParser.FindResults(data)
	if err != nil {
		return "", v1alpha1.AnalysisPhaseError, fmt.Errorf("Could not find JSONPath in body: %s", err)
	}
	val, valString, err := getValue(fullResults)
	if err != nil {
		return "", v1alpha1.AnalysisPhaseError, err
	}

	status, err := evaluate.EvaluateResult(val, metric, p.logCtx)
	return valString, status, err
}

func getValue(fullResults [][]reflect.Value) (any, string, error) {
	for _, results := range fullResults {
		for _, r := range results {
			val := r.Interface()
			valBytes, err := json.Marshal(val)
			return val, string(valBytes), err
		}
	}
	return nil, "", errors.New("result of web metric produced no value")
}

// Resume should not be used the WebMetric provider since all the work should occur in the Run method
func (p *Provider) Resume(run *v1alpha1.AnalysisRun, metric v1alpha1.Metric, measurement v1alpha1.Measurement) v1alpha1.Measurement {
	p.logCtx.Warn("WebMetric provider should not execute the Resume method")
	return measurement
}

// Terminate should not be used the WebMetric provider since all the work should occur in the Run method
func (p *Provider) Terminate(run *v1alpha1.AnalysisRun, metric v1alpha1.Metric, measurement v1alpha1.Measurement) v1alpha1.Measurement {
	p.logCtx.Warn("WebMetric provider should not execute the Terminate method")
	return measurement
}

// GarbageCollect is a no-op for the WebMetric provider
func (p *Provider) GarbageCollect(run *v1alpha1.AnalysisRun, metric v1alpha1.Metric, limit int) error {
	return nil
}

var insecureTransport *http.Transport = &http.Transport{
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
}

func NewWebMetricHttpClient(metric v1alpha1.Metric) (*http.Client, error) {
	var timeout time.Duration
	var oauthCfg clientcredentials.Config

	// Using a default timeout of 10 seconds
	if metric.Provider.Web.TimeoutSeconds <= 0 {
		timeout = time.Duration(10) * time.Second
	} else {
		timeout = time.Duration(metric.Provider.Web.TimeoutSeconds) * time.Second
	}

	c := &http.Client{
		Timeout: timeout,
	}
	if metric.Provider.Web.Insecure {
		c.Transport = insecureTransport
	}
	if metric.Provider.Web.Authentication.OAuth2.TokenURL != "" {
		if metric.Provider.Web.Authentication.OAuth2.ClientID == "" || metric.Provider.Web.Authentication.OAuth2.ClientSecret == "" {
			return nil, errors.New("missing mandatory parameter in metric for OAuth2 setup")
		}
		oauthCfg = clientcredentials.Config{
			ClientID:     metric.Provider.Web.Authentication.OAuth2.ClientID,
			ClientSecret: metric.Provider.Web.Authentication.OAuth2.ClientSecret,
			TokenURL:     metric.Provider.Web.Authentication.OAuth2.TokenURL,
			Scopes:       metric.Provider.Web.Authentication.OAuth2.Scopes,
		}
		return oauthCfg.Client(context.WithValue(context.Background(), oauth2.HTTPClient, c)), nil
	}
	return c, nil
}

func NewWebMetricJsonParser(metric v1alpha1.Metric) (*jsonpath.JSONPath, error) {
	jsonParser := jsonpath.New("metrics")
	jsonPath := metric.Provider.Web.JSONPath
	if jsonPath == "" {
		jsonPath = "{$}"
	}
	err := jsonParser.Parse(jsonPath)
	return jsonParser, err
}

func NewWebMetricProvider(logCtx log.Entry, client *http.Client, jsonParser *jsonpath.JSONPath) *Provider {
	return &Provider{
		logCtx:     logCtx,
		client:     client,
		jsonParser: jsonParser,
	}
}
