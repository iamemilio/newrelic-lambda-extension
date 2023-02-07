package telemetryApi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/secretsmanager"
	"github.com/aws/aws-sdk-go/service/secretsmanager/secretsmanageriface"
	"github.com/pkg/errors"

	"github.com/google/uuid"

	"newrelic-lambda-extension/util"
)

const (
	LogEndpointEU string = "https://log-api.eu.newrelic.com/log/v1"
	LogEndpointUS string = "https://log-api.newrelic.com/log/v1"

	MetricsEndpointEU string = "https://metric-api.eu.newrelic.com/metric/v1"
	MetricsEndpointUS string = "https://metric-api.newrelic.com/metric/v1"

	EventsEndpointEU string = "https://insights-collector.eu01.nr-data.net/v1/accounts/"
	EventsEndpointUS string = "https://insights-collector.newrelic.com/v1/accounts/"

	TracesEndpointEU string = "https://trace-api.eu.newrelic.com/trace/v1"
	TracesEndpointUS string = "https://trace-api.newrelic.com/trace/v1"

	maxLogMsgLen         = 4094 + 10000 // maximum blob size
	maxPayloadSizeBytes  = 1000000
	MaxAttributeValueLen = 4094
)

var (
	sess = session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	secrets secretsmanageriface.SecretsManagerAPI
)

type licenseKeySecret struct {
	LicenseKey string
}

func init() {
	secrets = secretsmanager.New(sess)
}

func decodeLicenseKey(rawJson *string) (string, error) {
	var lks licenseKeySecret

	err := json.Unmarshal([]byte(*rawJson), &lks)
	if err != nil {
		return "", err
	}
	if lks.LicenseKey == "" {
		return "", fmt.Errorf("malformed license key secret; missing \"LicenseKey\" attribute")
	}

	return lks.LicenseKey, nil
}

func getNewRelicLicenseKey(ctx context.Context) (string, error) {
	sId := "NEW_RELIC_LICENSE_KEY"
	v := os.Getenv("NEW_RELIC_LICENSE_KEY_SECRET")
	if len(v) > 0 {
		sId = v
	}
	l.Debugf("fetching secret with name or ARN: %s", sId)
	secretValueInput := secretsmanager.GetSecretValueInput{SecretId: &sId}
	secretValueOutput, err := secrets.GetSecretValueWithContext(ctx, &secretValueInput)
	if err != nil {
		return "", err
	}
	return decodeLicenseKey(secretValueOutput.SecretString)
}

func getEndpointURL(licenseKey string, typ string, EndpointOverride string) string {
	if EndpointOverride != "" {
		return EndpointOverride
	}
	switch typ {
	case "logging":
		if strings.HasPrefix(licenseKey, "eu") {
			return LogEndpointEU
		} else {
			return LogEndpointUS
		}
	case "metrics":
		if strings.HasPrefix(licenseKey, "eu") {
			return MetricsEndpointEU
		} else {
			return MetricsEndpointUS
		}
	case "events":
		if strings.HasPrefix(licenseKey, "eu") {
			return EventsEndpointEU
		} else {
			return EventsEndpointUS
		}
	case "traces":
		if strings.HasPrefix(licenseKey, "eu") {
			return TracesEndpointEU
		} else {
			return TracesEndpointUS
		}
	}
	return ""
}

func buildPayloads(ctx context.Context, logEntries []interface{}, d *Dispatcher, RequestID string) (map[string][]map[string]interface{}, error) {
	startBuild := time.Now()
	extension_name := util.Name

	// NB "." is not allowed in NR eventType
	var replacer = strings.NewReplacer(".", "_")

	data := make(map[string][]map[string]interface{})
	data["events"] = []map[string]interface{}{}
	data["traces"] = []map[string]interface{}{}
	data["logging"] = []map[string]interface{}{}
	data["metrics"] = []map[string]interface{}{}

	// current logic - terminate processing on an error, can be changed later
	for _, event := range logEntries {
		msInt, err := time.Parse(time.RFC3339, event.(LambdaTelemetryEvent).Time)
		if err != nil {
			return nil, err
		}
		// events
		data["events"] = append(data["events"], map[string]interface{}{
			"timestamp": msInt.UnixMilli(),
			/*"plugin":               util.Id,
			"faas.arn":             d.arn,
			"faas.name":            d.functionName, */
			"eventType":            "TelemetryApiEvent",
			"extension.name":       extension_name,
			"extension.version":    util.Version,
			"lambda.name":          d.functionName,
			"lambda.logevent.type": replacer.Replace(event.(LambdaTelemetryEvent).Type),
		})
		// logs
		if event.(LambdaTelemetryEvent).Record != nil {
			msg := fmt.Sprint(event.(LambdaTelemetryEvent).Record)
			if len(msg) > maxLogMsgLen {
				msg = msg[:maxLogMsgLen]
			}
			data["logging"] = append(data["logging"], map[string]interface{}{
				"timestamp": msInt.UnixMilli(),
				"message":   msg,
				"attributes": map[string]interface{}{
					"plugin": util.Id,
					"entity": map[string]string{
						"name": d.functionName,
					},
					"faas": map[string]string{
						"arn":  d.arn,
						"name": d.functionName,
					},
					"aws": map[string]string{
						"lambda.logevent.type": event.(LambdaTelemetryEvent).Type,
						"extension.name":       extension_name,
						"extension.version":    util.Version,
						"lambda.name":          d.functionName,
						"lambda.arn":           d.arn,
						"requestId":            RequestID,
					},
				},
			})

			if reflect.ValueOf(event.(LambdaTelemetryEvent).Record).Kind() == reflect.Map {
				eventRecord := event.(LambdaTelemetryEvent).Record.(map[string]interface{})
				// metrics
				rid := ""
				if v, okk := eventRecord["requestId"].(string); okk {
					rid = v
				}
				if val, ok := eventRecord["metrics"].(map[string]interface{}); ok {
					for key := range val {
						data["metrics"] = append(data["metrics"], map[string]interface{}{
							"name":      "aws.telemetry.lambda_ext." + key,
							"value":     val[key],
							"timestamp": msInt.UnixMilli(),
							"attributes": map[string]interface{}{
								"plugin":               util.Id,
								"faas.arn":             d.arn,
								"faas.name":            d.functionName,
								"lambda.logevent.type": event.(LambdaTelemetryEvent).Type,
								"requestId":            rid,
								"extension.name":       d.functionName,
								"extension.version":    util.Version,
								"lambda.name":          d.functionName,
							},
						})
					}
				}
				// spans
				if val, ok := eventRecord["spans"].([]interface{}); ok {
					for _, span := range val {
						attributes := make(map[string]interface{})
						attributes["event"] = event.(LambdaTelemetryEvent).Type
						attributes["service.name"] = extension_name
						var start time.Time
						for key, v := range span.(map[string]interface{}) {
							if key == "durationMs" {
								attributes["duration.ms"] = v.(float64)
							} else if key == "start" {
								start, err = time.Parse(time.RFC3339, v.(string))
								if err != nil {
									return nil, err
								}
							} else {
								attributes[key] = v.(string)
							}
						}
						el := map[string]interface{}{
							"trace.id":   rid,
							"timestamp":  start.UnixMilli(),
							"id":         (uuid.New()).String(),
							"attributes": attributes,
						}
						data["traces"] = append(data["traces"], el)
					}
				}
			}
		}
	}
	l.Debugf("[telemetryApi:buildPayloads] telemetry api payload objects built in: %s", time.Since(startBuild).String())
	return data, nil
}

func marshalAndCompressData(d *Dispatcher, data []map[string]interface{}, dataType string) ([]*bytes.Buffer, error) {
	bodyBytes, _ := json.Marshal(data)

	compressed, err := d.compressTool.Compress(bodyBytes)
	if err != nil {
		return nil, err
	}

	if compressed.Len() > maxPayloadSizeBytes {
		// Payload is too large, split in half, recursively
		split := len(data) / 2
		leftRet, err := marshalAndCompressData(d, data[0:split], dataType)
		if err != nil {
			return nil, err
		}

		rightRet, err := marshalAndCompressData(d, data[split:], dataType)
		if err != nil {
			return nil, err
		}

		return append(leftRet, rightRet...), nil
	}

	return []*bytes.Buffer{compressed}, nil
}

// please send compressed data
func sendData(ctx context.Context, d *Dispatcher, uri, dataType string, body *bytes.Buffer) error {
	startSend := time.Now()
	timeoutCtx, cancel := context.WithTimeout(ctx, util.SendToNewRelicTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(timeoutCtx, "POST", uri, body)
	if err != nil {
		return err
	}
	// the headers might be different for different endpoints
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Api-Key", d.licenseKey)
	req.Header.Set("Content-Encoding", "GZIP")
	if strings.Contains(uri, "trace") {
		req.Header.Set("Data-Format", "newrelic")
		req.Header.Set("Data-Format-Version", "1")
	}

	res, err := d.httpClient.Do(req)
	err = detecteErrorSendingBatch(res, err)
	if err != nil {
		l.Debugf("[telemetryApi:sendBatch] error occured while sending batch after %s", time.Since(startSend).String())
		return err
	}

	l.Debugf("[telemetryApi:sendBatch] took %s to send %s json", time.Since(startSend).String(), dataType)
	return err
}

func detecteErrorSendingBatch(response *http.Response, err error) error {
	if err != nil {
		return fmt.Errorf("[telemetryApi:sendBatch] Telemetry client error: %s", err)
	} else if response.StatusCode >= 300 {
		bytes, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		return fmt.Errorf("[telemtryApi:sendBatch] Telemetry client response: [%s] %s", response.Status, fmt.Sprint(bytes))
	}
	return nil
}

func sendDataToNR(ctx context.Context, logEntries []interface{}, d *Dispatcher, RequestID string) error {
	data, err := buildPayloads(ctx, logEntries, d, RequestID)
	if err != nil {
		return err
	}

	if len(data) > 0 {
		waitForSend := sync.WaitGroup{}
		errChan := make(chan error, 4)
		startAggregateSend := time.Now()

		// send logs
		if len(data["logging"]) > 0 {
			logPayloads, err := marshalAndCompressData(d, data["logging"], "logging")
			if err != nil {
				return err
			}

			for _, logPayload := range logPayloads {
				l.Debugf("[telemetryApi:sendDataToNR] sending %d compressed log payloads to new relic", len(logPayloads))
				waitForSend.Add(len(logPayloads))
				go func(logPayload *bytes.Buffer) {
					defer waitForSend.Done()
					err := sendData(ctx, d, getEndpointURL(d.licenseKey, "logging", ""), "logging", logPayload)
					if err != nil {
						errChan <- err
					}
				}(logPayload)
			}
		}
		// send metrics
		if len(data["metrics"]) > 0 {
			waitForSend.Add(1)
			startMarshal := time.Now()
			var dataMet []map[string][]map[string]interface{}
			dataMet = append(dataMet, map[string][]map[string]interface{}{
				"metrics": data["metrics"],
			})
			bodyBytes, _ := json.Marshal(dataMet)
			l.Debugf("[telemetryApi:sendDataToNR] took %s to marshal metrics json with %d metrics", time.Since(startMarshal).String(), len(data["metrics"]))
			l.Tracef("[telemetryApi:sendDataToNR] Metrics JSON: %s", string(bodyBytes))

			metricsPayload, err := d.compressTool.Compress(bodyBytes)
			if err != nil {
				return err
			}

			go func() {
				defer waitForSend.Done()
				err := sendData(ctx, d, getEndpointURL(d.licenseKey, "metrics", ""), "metrics", metricsPayload)
				if err != nil {
					errChan <- err
				}
			}()
		}
		// send events
		if len(data["events"]) > 0 {
			if len(d.accountID) > 0 {
				eventPayloads, err := marshalAndCompressData(d, data["events"], "logging")
				if err != nil {
					return err
				}

				l.Debugf("[telemetryApi:sendDataToNR] sending %d compressed log payloads to new relic", len(eventPayloads))
				waitForSend.Add(len(eventPayloads))
				for _, eventPayload := range eventPayloads {
					go func(eventPayload *bytes.Buffer) {
						defer waitForSend.Done()
						err := sendData(ctx, d, getEndpointURL(d.licenseKey, "events", "")+d.accountID+"/events", "events", eventPayload)
						if err != nil {
							errChan <- err
						}
					}(eventPayload)
				}
			} else {
				l.Warn("[telemetryApi:sendDataToNR] NEW_RELIC_ACCOUNT_ID is not set, therefore no events data sent")
			}
		}
		// send traces
		if len(data["traces"]) > 0 {
			waitForSend.Add(1)
			var dataTraces []map[string]interface{}
			dataTraces = append(dataTraces, map[string]interface{}{
				"common": map[string]map[string]string{
					"attributes": {
						"host":         "aws.amazon.com",
						"service.name": d.functionName,
					},
				},
				"spans": data["traces"],
			})
			startMarshal := time.Now()
			bodyBytes, _ := json.Marshal(dataTraces)
			l.Debugf("[telemetryApi:sendDataToNR] took %s to marshal traces json with %d traces", time.Since(startMarshal).String(), len(data["traces"]))
			l.Tracef("[telemetryApi:sendDataToNR] Traces JSON: %s", string(bodyBytes))

			tracePayload, err := d.compressTool.Compress(bodyBytes)

			go func() {
				defer waitForSend.Done()
				er := sendData(ctx, d, getEndpointURL(d.licenseKey, "traces", ""), "traces", tracePayload)
				if er != nil {
					errChan <- err
				}
			}()

		}

		l.Debugf("[telemetryApi:sendDataToNR] waiting for all payloads to send to new relic...")
		waitForSend.Wait()
		l.Debugf("[telemetryApi:sendDataToNR] waited %s for all Telemetry API payloads to concurrently send to new relic", time.Since(startAggregateSend).String())

		if len(errChan) > 0 {
			err = fmt.Errorf("%d errors occured while sending telemetry API payloads to New Relic", len(errChan))
			for i := 0; i < len(errChan); i++ {
				sendError := <-errChan
				errors.Wrap(err, sendError.Error())
			}
			return err
		}
	}

	return nil // success
}
