/**

Notes:
- 	Because of the asynchronous nature of the system, it is possible that telemetry for one invoke will be
	processed during the next invoke slice. Likewise, it is possible that telemetry for the last invoke will
	be processed during the SHUTDOWN event.

*/

package main

import (
	"context"
	"newrelic-lambda-extension/AwsLambdaExtension/agentTelemetry"
	"newrelic-lambda-extension/AwsLambdaExtension/extensionApi"
	"newrelic-lambda-extension/AwsLambdaExtension/telemetryApi"
	"os"
	"os/signal"
	"syscall"

	log "github.com/sirupsen/logrus"
)

var (
	l = log.WithFields(log.Fields{"pkg": "main"})
)

func main() {
	// Handle User Configured Settings
	conf := agentTelemetry.GetConfig()
	log.SetLevel(conf.LogLevel)

	l.Info("[main] Starting the New Relic Telemetry API extension")
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sigs
		cancel()
		l.Info("[main] Received", s)
		l.Info("[main] Exiting")
	}()

	// Step 1 - Register the extension with Extensions API
	l.Debug("[main] Registering extension")
	extensionApiClient := extensionApi.NewClient(conf.LogLevel)
	extensionId, err := extensionApiClient.Register(ctx, conf.ExtensionName)
	if err != nil {
		l.Fatal(err)
	}
	l.Debug("[main] Registation success with extensionId", extensionId)

	// Step 2 - Start the local http listener which will receive data from Telemetry API
	l.Debug("[main] Starting the Telemetry listener")
	telemetryListener := telemetryApi.NewTelemetryApiListener()
	telemetryListenerUri, err := telemetryListener.Start()
	if err != nil {
		l.Fatal(err)
	}

	// Step 3 - Subscribe the listener to Telemetry API
	l.Debug("[main] Subscribing to the Telemetry API")
	telemetryApiClient := telemetryApi.NewClient(conf.LogLevel)
	_, err = telemetryApiClient.Subscribe(ctx, extensionId, telemetryListenerUri)
	if err != nil {
		l.Fatal(err)
	}
	l.Debug("[main] Subscription success")
	dispatcher := telemetryApi.NewDispatcher(extensionApiClient.GetFunctionName(), &conf, ctx, conf.TelemetryAPIBatchSize)

	// Set up new relic agent telemetry client
	agentDispatcher := agentTelemetry.NewDispatcher(conf)

	l.Info("[main] New Relic Telemetry API Extension succesfully registered and subscribed")

	// Will block until invoke or shutdown event is received or cancelled via the context.
	for {
		select {
		case <-ctx.Done():
			return
		default:
			l.Debug("[main] Waiting for next event...")

			// This is a blocking action
			res, err := extensionApiClient.NextEvent(ctx)
			if err != nil {
				l.Errorf("[main] Exiting. Error: %v", err)
				return
			}
			l.Debugf("[main] Received event %+v", res)

			// Dispatching log events from previous invocations
			agentDispatcher.AddRequest(res)
			dispatcher.Dispatch(ctx, telemetryListener.LogEventsQueue, false)
			agentDispatcher.Dispatch(ctx, res, false)

			if res.EventType == extensionApi.Invoke {
				l.Debug("[handleInvoke]")
				// we no longer care about this but keep it here just in case
			} else if res.EventType == extensionApi.Shutdown {
				// force dispatch all remaining telemetry, handle shutdown
				l.Debug("[handleShutdown]")
				dispatcher.Dispatch(ctx, telemetryListener.LogEventsQueue, true)
				agentDispatcher.Dispatch(ctx, res, true)
				l.Info("[main] New Relic Telemetry API Extension successfully shut down")
				return
			}
		}
	}
}
