/**
 * Copyright 2017 Comcast Cable Communications Management, LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"time"

	"github.com/Comcast/webpa-common/concurrent"
	"github.com/Comcast/webpa-common/logging"
	"github.com/Comcast/webpa-common/secure"
	"github.com/Comcast/webpa-common/secure/handler"
	"github.com/Comcast/webpa-common/secure/key"
	"github.com/Comcast/webpa-common/server"
	"github.com/Comcast/webpa-common/webhook"
	"github.com/Comcast/webpa-common/webhook/aws"

	"github.com/Comcast/webpa-common/xmetrics"
	"github.com/SermoDigital/jose/jwt"
	"github.com/go-kit/kit/log"
	"github.com/gorilla/mux"
	"github.com/justinas/alice"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

//convenient global values
const (
	applicationName, apiBase = "tr1d1um", "/api/v2"

	DefaultKeyID            = "current"
	defaultClientTimeout    = "30s"
	defaultRespWaitTimeout  = "40s"
	defaultNetDialerTimeout = "5s"
	defaultRetryInterval    = "2s"
	defaultMaxRetries       = 2

	supportedServicesKey = "supportedServices"
	targetURLKey         = "targetURL"
	netDialerTimeoutKey  = "netDialerTimeout"
	clientTimeoutKey     = "clientTimeout"
	reqRetryIntervalKey  = "requestRetryInterval"
	reqMaxRetriesKey     = "requestMaxRetries"
	respWaitTimeoutKey   = "respWaitTimeout"
)

func tr1d1um(arguments []string) (exitCode int) {

	var (
		f                                   = pflag.NewFlagSet(applicationName, pflag.ContinueOnError)
		v                                   = viper.New()
		logger, metricsRegistry, webPA, err = server.Initialize(applicationName, arguments, f, v, webhook.Metrics, aws.Metrics, secure.Metrics)
	)

	// set config file value defaults
	v.SetDefault(clientTimeoutKey, defaultClientTimeout)
	v.SetDefault(respWaitTimeoutKey, defaultRespWaitTimeout)
	v.SetDefault(reqRetryIntervalKey, defaultRetryInterval)
	v.SetDefault(reqMaxRetriesKey, defaultMaxRetries)
	v.SetDefault(netDialerTimeoutKey, defaultNetDialerTimeout)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to initialize viper: %s\n", err.Error())
		return 1
	}

	var (
		infoLogger  = logging.Info(logger)
		errorLogger = logging.Error(logger)
	)

	infoLogger.Log("configurationFile", v.ConfigFileUsed())

	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to unmarshall config data into struct: %s\n", err.Error())
		return 1
	}

	preHandler, err := SetUpPreHandler(v, logger, metricsRegistry)

	if err != nil {
		fmt.Fprintf(os.Stderr, "error setting up prehandler: %s\n", err.Error())
		return 1
	}

	conversionHandler := SetUpHandler(v, logger)

	r := mux.NewRouter()
	baseRouter := r.PathPrefix(apiBase).Subrouter()

	AddRoutes(baseRouter, preHandler, conversionHandler)

	var snsFactory *webhook.Factory

	if accessKey := v.GetString("aws.accessKey"); accessKey != "" && accessKey != "fake-accessKey" { //only proceed if sure that value was set and not the default one
		if snsFactory, exitCode = ConfigureWebHooks(baseRouter, r, preHandler, v, logger, metricsRegistry); exitCode != 0 {
			return
		}
	}

	var (
		_, tr1d1umServer = webPA.Prepare(logger, nil, metricsRegistry, r)
		signals          = make(chan os.Signal, 1)
	)

	if snsFactory != nil {
		go snsFactory.PrepareAndStart()
	}

	//
	// Execute the runnable, which runs all the servers, and wait for a signal
	//
	waitGroup, shutdown, err := concurrent.Execute(tr1d1umServer)

	if err != nil {
		errorLogger.Log(logging.MessageKey(), "Unable to start tr1d1um", logging.ErrorKey(), err)
		return 4
	}

	signal.Notify(signals)
	s := server.SignalWait(infoLogger, signals, os.Kill, os.Interrupt)
	errorLogger.Log(logging.MessageKey(), "exiting due to signal", "signal", s)
	close(shutdown)
	waitGroup.Wait()

	return 0
}

//ConfigureWebHooks sets route paths, initializes and synchronizes hook registries for this tr1d1um instance
//baseRouter is pre-configured with the api/v2 prefix path
//root is the original router used by webHookFactory.Initialize()
func ConfigureWebHooks(baseRouter *mux.Router, root *mux.Router, preHandler *alice.Chain, v *viper.Viper, logger log.Logger, registry xmetrics.Registry) (*webhook.Factory, int) {
	webHookFactory, err := webhook.NewFactory(v)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating new webHook factory: %s\n", err)
		return nil, 1
	}

	webHookRegistry, webHookHandler := webHookFactory.NewRegistryAndHandler(registry)

	// register webHook end points for api
	baseRouter.Handle("/hook", preHandler.ThenFunc(webHookRegistry.UpdateRegistry))
	baseRouter.Handle("/hooks", preHandler.ThenFunc(webHookRegistry.GetRegistry))

	selfURL := &url.URL{
		Scheme: "https",
		Host:   v.GetString("fqdn") + v.GetString("primary.address"),
	}

	webHookFactory.Initialize(root, selfURL, webHookHandler, logger, registry, nil)
	return webHookFactory, 0
}

//AddRoutes configures the paths and connection rules to TR1D1UM
func AddRoutes(r *mux.Router, preHandler *alice.Chain, conversionHandler *ConversionHandler) {
	var BodyNonEmpty = func(request *http.Request, match *mux.RouteMatch) (accept bool) {
		if request.Body != nil {
			var tmp bytes.Buffer
			p, err := ioutil.ReadAll(request.Body)
			if accept = err == nil && len(p) > 0; accept {
				//place back request's body
				tmp.Write(p)
				request.Body = ioutil.NopCloser(&tmp)
			}
		}
		return
	}

	r.Handle("/device/{deviceid}/stat", preHandler.ThenFunc(conversionHandler.HandleStat)).
		Methods(http.MethodGet)

	r.Handle("/device/{deviceid}/{service}", preHandler.Then(conversionHandler)).
		Methods(http.MethodGet)

	r.Handle("/device/{deviceid}/{service}", preHandler.Then(conversionHandler)).
		Methods(http.MethodPatch)

	r.Handle("/device/{deviceid}/{service}/{parameter}", preHandler.Then(conversionHandler)).
		Methods(http.MethodDelete)

	r.Handle("/device/{deviceid}/{service:iot}", preHandler.ThenFunc(conversionHandler.HandleIOT)).
		Methods(http.MethodPost) //TODO: path is temporary. Should be deleted once endpoint is not needed in tr1d1um

	r.Handle("/device/{deviceid}/{service}/{parameter}", preHandler.Then(conversionHandler)).
		Methods(http.MethodPut, http.MethodPost).MatcherFunc(BodyNonEmpty)
}

//SetUpHandler prepares the main handler under TR1D1UM which is the ConversionHandler
func SetUpHandler(v *viper.Viper, logger log.Logger) (cHandler *ConversionHandler) {
	clientTimeout, _ := time.ParseDuration(v.GetString(clientTimeoutKey))
	respTimeout, _ := time.ParseDuration(v.GetString(respWaitTimeoutKey))
	retryInterval, _ := time.ParseDuration(v.GetString(reqRetryIntervalKey))
	dialerTimeout, _ := time.ParseDuration(v.GetString(netDialerTimeoutKey))
	maxRetries := v.GetInt(reqMaxRetriesKey)

	cHandler = &ConversionHandler{
		WdmpConvert: &ConversionWDMP{
			WRPSource: v.GetString("WRPSource")},

		Sender: &Tr1SendAndHandle{
			RespTimeout: respTimeout,
			Logger:      logger,
			client: &http.Client{Timeout: clientTimeout,
				Transport: &http.Transport{
					Dial: (&net.Dialer{
						Timeout: dialerTimeout,
					}).Dial}}},

		Logger: logger,

		RequestValidator: &TR1RequestValidator{
			supportedServices: getSupportedServicesMap(v.GetStringSlice(supportedServicesKey)),
			Logger:            logger,
		},

		RetryStrategy: RetryStrategyFactory{}.NewRetryStrategy(logger, retryInterval, maxRetries,
			ShouldRetryOnResponse, OnRetryInternalFailure),
		WRPRequestURL: fmt.Sprintf("%s%s/device", v.GetString(targetURLKey), apiBase),

		TargetURL: v.GetString(targetURLKey),
	}

	return
}

//SetUpPreHandler configures the authorization requirements for requests to reach the main handler
func SetUpPreHandler(v *viper.Viper, logger log.Logger, registry xmetrics.Registry) (preHandler *alice.Chain, err error) {
	m := secure.NewJWTValidationMeasures(registry)
	var validator secure.Validator
	if validator, err = getValidator(v, m); err == nil {

		authHandler := handler.AuthorizationHandler{
			HeaderName:          "Authorization",
			ForbiddenStatusCode: 403,
			Validator:           validator,
			Logger:              logger,
		}

		authHandler.DefineMeasures(m)

		newPreHandler := alice.New(authHandler.Decorate)
		preHandler = &newPreHandler
	}
	return
}

//getValidator returns a validator for JWT/Basic tokens
//It reads in tokens from a config file. Zero or more tokens
//can be read.
func getValidator(v *viper.Viper, m *secure.JWTValidationMeasures) (validator secure.Validator, err error) {
	var jwtVals []JWTValidator

	v.UnmarshalKey("jwtValidators", &jwtVals)

	// if a JWTKeys section was supplied, configure a JWS validator
	// and append it to the chain of validators
	validators := make(secure.Validators, 0, len(jwtVals))

	for _, validatorDescriptor := range jwtVals {
		validatorDescriptor.Custom.DefineMeasures(m)

		var keyResolver key.Resolver
		keyResolver, err = validatorDescriptor.Keys.NewResolver()
		if err != nil {
			validator = validators
			return
		}

		validator := secure.JWSValidator{
			DefaultKeyId:  DefaultKeyID,
			Resolver:      keyResolver,
			JWTValidators: []*jwt.Validator{validatorDescriptor.Custom.New()},
		}

		validator.DefineMeasures(m)
		validators = append(validators, validator)
	}

	basicAuth := v.GetStringSlice("authHeader")
	for _, authValue := range basicAuth {
		validators = append(
			validators,
			secure.ExactMatchValidator(authValue),
		)
	}

	validator = validators

	return
}

func getSupportedServicesMap(supportedServices []string) (supportedServicesMap map[string]struct{}) {
	supportedServicesMap = map[string]struct{}{}
	if supportedServices != nil {
		for _, supportedService := range supportedServices {
			supportedServicesMap[supportedService] = struct{}{}
		}
	}
	return
}

func main() {
	os.Exit(tr1d1um(os.Args))
}
