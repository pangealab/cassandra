// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"cloud.google.com/go/profiler"
	// "contrib.go.opencensus.io/exporter/stackdriver"
	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"go.opencensus.io/plugin/ocgrpc"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gorilla/mux/otelmux"
	// "go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	// "go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	// "go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.7.0"
	// "go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/encoding/gzip"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	port            = "8080"
	defaultCurrency = "USD"
	cookieMaxAge    = 60 * 60 * 48

	cookiePrefix    = "shop_"
	cookieSessionID = cookiePrefix + "session-id"
	cookieCurrency  = cookiePrefix + "currency"
)

var (
	whitelistedCurrencies = map[string]bool{
		"USD": true,
		"EUR": true,
		"CAD": true,
		"JPY": true,
		"GBP": true,
		"TRY": true}

	reg = prometheus.NewRegistry()
	requestCount = promauto.With(reg).NewCounterVec(
		prometheus.CounterOpts{
			Name: "example_requests_total",
			Help: "Total number of HTTP requests by status code and method.",
		},
		[]string{"code", "method"},
	)		
)

type ctxKeySessionID struct{}

type frontendServer struct {
	productCatalogSvcAddr string
	productCatalogSvcConn *grpc.ClientConn

	currencySvcAddr string
	currencySvcConn *grpc.ClientConn

	cartSvcAddr string
	cartSvcConn *grpc.ClientConn

	recommendationSvcAddr string
	recommendationSvcConn *grpc.ClientConn

	checkoutSvcAddr string
	checkoutSvcConn *grpc.ClientConn

	shippingSvcAddr string
	shippingSvcConn *grpc.ClientConn

	adSvcAddr string
	adSvcConn *grpc.ClientConn
}

func main() {
	ctx := context.Background()
	log := logrus.New()
	log.Level = logrus.DebugLevel
	log.Formatter = &logrus.JSONFormatter{
		FieldMap: logrus.FieldMap{
			logrus.FieldKeyTime:  "timestamp",
			logrus.FieldKeyLevel: "severity",
			logrus.FieldKeyMsg:   "message",
		},
		TimestampFormat: time.RFC3339Nano,
	}
	log.Out = os.Stdout

	if os.Getenv("DISABLE_TRACING") == "" {
		log.Info("Tracing enabled.")
		ls_access_token, _ := os.LookupEnv("LS_ACCESS_TOKEN")

		//Define system resource
		resource := resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String("frontEnd"),
		)
		//OTLP trace exporter
		exporter, err := otlptrace.New(ctx, otlptracegrpc.NewClient(
			otlptracegrpc.WithTLSCredentials(credentials.NewClientTLSFromCert(nil, "")),
			otlptracegrpc.WithEndpoint("ingest.lightstep.com:443"),
			otlptracegrpc.WithHeaders(map[string]string{"lightstep-access-token":ls_access_token}),
			otlptracegrpc.WithCompressor(gzip.Name),),
		)
		if err != nil {
			log.Fatalf("Could not start web server: %s", err)
		}

		// Define TracerProvider
		tracerProvider := sdktrace.NewTracerProvider(
			sdktrace.WithSampler(sdktrace.AlwaysSample()),
			sdktrace.WithResource(resource),
			sdktrace.WithBatcher(exporter),
		)

		// Set TracerProvider
		otel.SetTracerProvider(tracerProvider)
	} else {
		log.Info("Tracing disabled.")
	}

	if os.Getenv("DISABLE_PROFILER") == "" {
		log.Info("Profiling enabled.")
		go initProfiling(log, "frontend", "1.0.0")
	} else {
		log.Info("Profiling disabled.")
	}

	srvPort := port
	if os.Getenv("PORT") != "" {
		srvPort = os.Getenv("PORT")
	}
	addr := os.Getenv("LISTEN_ADDR")
	svc := new(frontendServer)
	mustMapEnv(&svc.productCatalogSvcAddr, "PRODUCT_CATALOG_SERVICE_ADDR")
	mustMapEnv(&svc.currencySvcAddr, "CURRENCY_SERVICE_ADDR")
	mustMapEnv(&svc.cartSvcAddr, "CART_SERVICE_ADDR")
	mustMapEnv(&svc.recommendationSvcAddr, "RECOMMENDATION_SERVICE_ADDR")
	mustMapEnv(&svc.checkoutSvcAddr, "CHECKOUT_SERVICE_ADDR")
	mustMapEnv(&svc.shippingSvcAddr, "SHIPPING_SERVICE_ADDR")
	mustMapEnv(&svc.adSvcAddr, "AD_SERVICE_ADDR")

	mustConnGRPC(ctx, &svc.currencySvcConn, svc.currencySvcAddr)
	mustConnGRPC(ctx, &svc.productCatalogSvcConn, svc.productCatalogSvcAddr)
	mustConnGRPC(ctx, &svc.cartSvcConn, svc.cartSvcAddr)
	mustConnGRPC(ctx, &svc.recommendationSvcConn, svc.recommendationSvcAddr)
	mustConnGRPC(ctx, &svc.shippingSvcConn, svc.shippingSvcAddr)
	mustConnGRPC(ctx, &svc.checkoutSvcConn, svc.checkoutSvcAddr)
	mustConnGRPC(ctx, &svc.adSvcConn, svc.adSvcAddr)

	r := mux.NewRouter()
	r.Use(otelmux.Middleware("frontEnd"))
	r.Path("/").Handler(promhttp.InstrumentHandlerCounter(requestCount, http.HandlerFunc(svc.homeHandler))).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/product/{id}", svc.productHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/cart", svc.viewCartHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/cart", svc.addToCartHandler).Methods(http.MethodPost)
	r.HandleFunc("/cart/empty", svc.emptyCartHandler).Methods(http.MethodPost)
	r.HandleFunc("/setCurrency", svc.setCurrencyHandler).Methods(http.MethodPost)
	r.HandleFunc("/logout", svc.logoutHandler).Methods(http.MethodGet)
	r.HandleFunc("/cart/checkout", svc.placeOrderHandler).Methods(http.MethodPost)
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir("./static/"))))
	r.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "User-agent: *\nDisallow: /") })
	r.HandleFunc("/_healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") })
	r.Path("/metrics").Handler(promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	var handler http.Handler = r
	handler = &logHandler{log: log, next: handler} // add logging
	handler = ensureSessionID(handler)             // add session ID
	// handler = &ochttp.Handler{                     // add opencensus instrumentation
	// 	Handler:     handler,
	// 	Propagation: &b3.HTTPFormat{}}

	log.Infof("starting server on " + addr + ":" + srvPort)
	log.Fatal(http.ListenAndServe(addr+":"+srvPort, handler))
}

// func initJaegerTracing(log logrus.FieldLogger) {

// 	svcAddr := os.Getenv("JAEGER_SERVICE_ADDR")
// 	if svcAddr == "" {
// 		log.Info("jaeger initialization disabled.")
// 		return
// 	}

// 	// Register the Jaeger exporter to be able to retrieve
// 	// the collected spans.
// 	exporter, err := jaeger.NewExporter(jaeger.Options{
// 		Endpoint: fmt.Sprintf("http://%s", svcAddr),
// 		Process: jaeger.Process{
// 			ServiceName: "frontend",
// 		},
// 	})
// 	if err != nil {
// 		log.Fatal(err)
// 	}
// 	trace.RegisterExporter(exporter)
// 	log.Info("jaeger initialization completed.")
// }


// func initLSTracer(log logrus.FieldLogger) {
// 	var tracer = otel.Tracer("frontEnd")

// 	ls_access_token, _ :=os.LookupEnv("LS_ACCESS_TOKEN")

// 	//OTLP trace exporter
// 	exporter, err := otlptracegrpc.New(context.Context, otlptracegrpc.NewClient(
// 		otlptracegrpc.WithTLSCredentials(credentials.NewClientTLSFromCert(nil, "")),
// 		otlptracegrpc.WithEndpoint("ingest.lightstep.com:443"),
// 		otlptracegrpc.WithHeaders(map[string]string{"lightstep-access-token":ls_access_token}),
// 		otlptracegrpc.WithCompressor(gzip.Name),),
// 	)
// 	resource := resource.NewWithAttributes(
// 	semconv.SchemaURL,
// 	semconv.ServiceNameKey.String("frontEnd"),
// 	semconv.ServiceVersionKey.String("1.0.0"),
// 	)

// 	// Define TracerProvider
// 	tracerProvider := sdktrace.NewTracerProvider(
// 		sdktrace.WithSampler(sdktrace.AlwaysSample()),
// 		sdktrace.WithBatcher(exporter),
// 		sdktrace.WithResource(newResource()),
// 	)

// 	// Set TracerProvider
// 	otel.SetTracerProvider(tracerProvider)

// 	//Set Propagation headers
// 	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

// }

// func initStats(log logrus.FieldLogger, exporter *stackdriver.Exporter) {
// 	view.SetReportingPeriod(60 * time.Second)
// 	view.RegisterExporter(exporter)
// 	if err := view.Register(ochttp.DefaultServerViews...); err != nil {
// 		log.Warn("Error registering http default server views")
// 	} else {
// 		log.Info("Registered http default server views")
// 	}
// 	if err := view.Register(ocgrpc.DefaultClientViews...); err != nil {
// 		log.Warn("Error registering grpc default client views")
// 	} else {
// 		log.Info("Registered grpc default client views")
// 	}
// }

// func initStackdriverTracing(log logrus.FieldLogger) {
// 	// TODO(ahmetb) this method is duplicated in other microservices using Go
// 	// since they are not sharing packages.
// 	for i := 1; i <= 3; i++ {
// 		log = log.WithField("retry", i)
// 		exporter, err := stackdriver.NewExporter(stackdriver.Options{})
// 		if err != nil {
// 			// log.Warnf is used since there are multiple backends (stackdriver & jaeger)
// 			// to store the traces. In production setup most likely you would use only one backend.
// 			// In that case you should use log.Fatalf.
// 			log.Warnf("failed to initialize Stackdriver exporter: %+v", err)
// 		} else {
// 			trace.RegisterExporter(exporter)
// 			log.Info("registered Stackdriver tracing")

// 			// Register the views to collect server stats.
// 			initStats(log, exporter)
// 			return
// 		}
// 		d := time.Second * 20 * time.Duration(i)
// 		log.Debugf("sleeping %v to retry initializing Stackdriver exporter", d)
// 		time.Sleep(d)
// 	}
// 	log.Warn("could not initialize Stackdriver exporter after retrying, giving up")
// }

// func initTracing(log logrus.FieldLogger) {
// 	// This is a demo app with low QPS. trace.AlwaysSample() is used here
// 	// to make sure traces are available for observation and analysis.
// 	// In a production environment or high QPS setup please use
// 	// trace.ProbabilitySampler set at the desired probability.
// 	trace.ApplyConfig(trace.Config{DefaultSampler: trace.AlwaysSample()})

// 	initJaegerTracing(log)
// 	initStackdriverTracing(log)

// }

func initProfiling(log logrus.FieldLogger, service, version string) {
	// TODO(ahmetb) this method is duplicated in other microservices using Go
	// since they are not sharing packages.
	for i := 1; i <= 3; i++ {
		log = log.WithField("retry", i)
		if err := profiler.Start(profiler.Config{
			Service:        service,
			ServiceVersion: version,
			// ProjectID must be set if not running on GCP.
			// ProjectID: "my-project",
		}); err != nil {
			log.Warnf("warn: failed to start profiler: %+v", err)
		} else {
			log.Info("started Stackdriver profiler")
			return
		}
		d := time.Second * 10 * time.Duration(i)
		log.Debugf("sleeping %v to retry initializing Stackdriver profiler", d)
		time.Sleep(d)
	}
	log.Warn("warning: could not initialize Stackdriver profiler after retrying, giving up")
}

func mustMapEnv(target *string, envKey string) {
	v := os.Getenv(envKey)
	if v == "" {
		panic(fmt.Sprintf("environment variable %q not set", envKey))
	}
	*target = v
}

func mustConnGRPC(ctx context.Context, conn **grpc.ClientConn, addr string) {
	var err error
	ctx, cancel := context.WithTimeout(ctx, time.Second*3)
	defer cancel()
	*conn, err = grpc.DialContext(ctx, addr,
		grpc.WithInsecure(),
		grpc.WithStatsHandler(&ocgrpc.ClientHandler{}))
	if err != nil {
		panic(errors.Wrapf(err, "grpc: failed to connect %s", addr))
	}
}
