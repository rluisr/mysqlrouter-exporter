package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/jessevdk/go-flags"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rluisr/mysqlrouter-go"
)

var (
	mysqlRouterClient *mysqlrouter.Client
	collectInterval   = 2 * time.Second // default collect interval override with --collect-interval

	version string
	commit  string
	date    string
)

var args struct {
	RestAPIURL    string `short:"" long:"url" required:"true" env:"MYSQLROUTER_EXPORTER_URL" description:"MySQL Router Rest API URL"`
	RestAPIUser   string `short:"" long:"user" required:"false" env:"MYSQLROUTER_EXPORTER_USER" description:"Username for REST API"`
	RestAPIPass   string `short:"" long:"pass" required:"false" env:"MYSQLROUTER_EXPORTER_PASS" description:"Password for REST API"`
	ListenPort    int    `short:"p" long:"listen-port" default:"9152" description:"Listen port"`
	ServiceName   string `short:"" long:"service-name" required:"true" env:"MYSQLROUTER_EXPORTER_SERVICE_NAME" description:"Service name for MySQL Router"`
	TLSCACertPath string `short:"" long:"tls-ca-cert-path" required:"false" env:"MYSQLROUTER_TLS_CACERT_PATH" description:"TLS CA cacert path"`
	TLSCertPath   string `short:"" long:"tls-cert-path" required:"false" env:"MYSQLROUTER_TLS_CERT_PATH" description:"TLS cert path"`
	TLSKeyPath    string `short:"" long:"tls-key-path" required:"false" env:"MYSQLROUTER_TLS_KEY_PATH" description:"TLS key path"`
	SkipTLSVerify bool   `short:"k" long:"skip-tls-verify" description:"Skip TLS Verification"`

	CollectInterval                               int  `short:"" long:"collect-interval" required:"false" default:"2" description:"Collect interval time in sec."`
	CollectMetadataStatus                         bool `short:"" long:"collect.metadata.status" description:"Collect metrics from metadata status. CPU usage will increase."`
	CollectRouteConnectionsByteFromServer         bool `short:"" long:"collect.route.connections.byte_from_server" description:"Collect metrics from route connections. CPU usage will increase."`
	CollectRouteConnectionsByteToServer           bool `short:"" long:"collect.route.connections.byte_to_server" description:"Collect metrics from route connections. CPU usage will increase."`
	CollectRouteConnectionsTimeStarted            bool `short:"" long:"collect.route.connections.time_started" description:"Collect metrics from route connections. CPU usage will increase."`
	CollectRouteConnectionsTimeConnectedToServer  bool `short:"" long:"collect.route.connections.time_connected_to_server" description:"Collect metrics from route connections. CPU usage will increase."`
	CollectRouteConnectionsTimeLastSentToServer   bool `short:"" long:"collect.route.connections.time_last_sent_to_server" description:"Collect metrics from route connections. CPU usage will increase."`
	CollectRouteConnectionsTimeReceivedFromServer bool `short:"" long:"collect.route.connections.time_received_from_server" description:"Collect metrics from route connections. CPU usage will increase."`

	Version bool `short:"v" long:"version" description:"Show version"`
}

var (
	lastRouter           *mysqlrouter.Router
	lastRoutes           []*mysqlrouter.Routes
	lastRouteConnections []*mysqlrouter.RouteConnections
)

const (
	nameSpace = "mysqlrouter"
)

func initialClient() (*mysqlrouter.Client, error) {
	if args.RestAPIURL == "" {
		panic("These environments are missing.\n" +
			"MYSQLROUTER_EXPORTER_URL is required and MYSQLROUTER_EXPORTER_USER and MYSQLROUTER_EXPORTER_PASS are optional.")
	}

	opts, err := initializeClientOptions()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize client options err: %w", err)
	}

	return mysqlrouter.New(args.RestAPIURL, args.RestAPIUser, args.RestAPIPass, opts)
}

func initializeClientOptions() (*mysqlrouter.Options, error) {
	if args.SkipTLSVerify {
		return &mysqlrouter.Options{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}, nil
	}

	if args.TLSCACertPath == "" && args.TLSCertPath == "" && args.TLSKeyPath == "" && !args.SkipTLSVerify {
		return nil, nil
	}

	certPath, err := filepath.Abs(args.TLSCertPath)
	if err != nil {
		return nil, err
	}
	keyPath, err := filepath.Abs(args.TLSKeyPath)
	if err != nil {
		return nil, err
	}
	caPath, err := filepath.Abs(args.TLSCACertPath)
	if err != nil {
		return nil, err
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}

	caCert, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caCertPool,
	}

	opts := &mysqlrouter.Options{
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
	}

	return opts, nil
}

func recordMetrics() {
	go func() {
		for {
			collectMetrics()
			time.Sleep(collectInterval)
		}
	}()
}

func collectMetrics() {
	router, err := collectRouterMetrics()
	if err != nil {
		writeError(err)
		router = nil
	}

	collectMetadataMetrics(router)
	collectRouteMetrics(router)
}

func collectRouterMetrics() (*mysqlrouter.Router, error) {
	router, err := mysqlRouterClient.GetRouterStatus()
	if err != nil {
		routerUpGauge.Set(0)
		return nil, err
	}

	routerUpGauge.Set(1)
	routerStatusGauge.WithLabelValues(strconv.Itoa(router.ProcessID), router.ProductEdition, router.TimeStarted.String(), router.Version, router.Hostname, args.ServiceName)

	lastRouter = router

	return router, nil
}

func collectMetadataMetrics(router *mysqlrouter.Router) {
	// nil means router is down
	// so we don't need to collect metadata metrics
	if router == nil {
		return
	}

	metadata, err := mysqlRouterClient.GetAllMetadata()
	if err != nil {
		writeError(err)
		return
	}

	for _, m := range metadata {
		metadataGauge.WithLabelValues(m.Name)

		// config
		metadataConfig, gmcErr := mysqlRouterClient.GetMetadataConfig(m.Name)
		if gmcErr != nil {
			writeError(gmcErr)
			return
		}
		metadataConfigGauge.WithLabelValues(m.Name, metadataConfig.ClusterName, strconv.Itoa(metadataConfig.TimeRefreshInMs), metadataConfig.GroupReplicationID)

		// config nodes count
		metadataConfigNodesGauge.WithLabelValues(m.Name, router.Hostname, metadataConfig.ClusterName).Set(float64(len(metadataConfig.Nodes)))

		// status
		if args.CollectMetadataStatus {
			metadataStatus, gmsErr := mysqlRouterClient.GetMetadataStatus(m.Name)
			if gmsErr != nil {
				writeError(gmsErr)
				return
			}
			metadataStatusGauge.Reset()
			metadataStatusGauge.WithLabelValues(m.Name, strconv.Itoa(metadataStatus.RefreshFailed), metadataStatus.TimeLastRefreshSucceeded.String(), metadataStatus.LastRefreshHostname, strconv.Itoa(metadataStatus.LastRefreshPort))
		}
	}
}

func collectRouteMetrics(router *mysqlrouter.Router) {
	// nil means router is down
	// so route metrics will be 0
	if router == nil {
		for _, route := range lastRoutes {
			routeActiveConnectionsGauge.WithLabelValues(route.Name, lastRouter.Hostname).Set(0)
			routeTotalConnectionsGauge.WithLabelValues(route.Name, lastRouter.Hostname).Set(0)
			routeBlockedHostsGauge.WithLabelValues(route.Name, lastRouter.Hostname).Set(0)
			routeHealthGauge.WithLabelValues(route.Name, lastRouter.Hostname).Set(0)

			for _, routeConnection := range lastRouteConnections {
				if args.CollectRouteConnectionsByteFromServer {
					routeConnectionsByteFromServerGauge.WithLabelValues(route.Name, router.Hostname, routeConnection.SourceAddress, routeConnection.DestinationAddress).Set(0)
				}
				if args.CollectRouteConnectionsByteToServer {
					routeConnectionsByteToServerGauge.WithLabelValues(route.Name, router.Hostname, routeConnection.SourceAddress, routeConnection.DestinationAddress).Set(0)
				}
				if args.CollectRouteConnectionsTimeStarted {
					routeConnectionsTimeStartedGauge.WithLabelValues(route.Name, router.Hostname, routeConnection.SourceAddress, routeConnection.DestinationAddress).Set(0)
				}
				if args.CollectRouteConnectionsTimeConnectedToServer {
					routeConnectionsTimeConnectedToServerGauge.WithLabelValues(route.Name, router.Hostname, routeConnection.SourceAddress, routeConnection.DestinationAddress).Set(0)
				}
				if args.CollectRouteConnectionsTimeLastSentToServer {
					routeConnectionsTimeLastSentToServerGauge.WithLabelValues(route.Name, router.Hostname, routeConnection.SourceAddress, routeConnection.DestinationAddress).Set(0)
				}
				if args.CollectRouteConnectionsTimeReceivedFromServer {
					routeConnectionsTimeLastReceivedFromServerGauge.WithLabelValues(route.Name, router.Hostname, routeConnection.SourceAddress, routeConnection.DestinationAddress).Set(0)
				}
			}
		}
		return
	}

	routes, err := mysqlRouterClient.GetAllRoutes()
	if err != nil {
		writeError(err)
		return
	}

	if args.CollectRouteConnectionsByteFromServer {
		routeConnectionsByteFromServerGauge.Reset()
	}
	if args.CollectRouteConnectionsByteToServer {
		routeConnectionsByteToServerGauge.Reset()
	}
	if args.CollectRouteConnectionsTimeStarted {
		routeConnectionsTimeStartedGauge.Reset()
	}
	if args.CollectRouteConnectionsTimeConnectedToServer {
		routeConnectionsTimeConnectedToServerGauge.Reset()
	}
	if args.CollectRouteConnectionsTimeLastSentToServer {
		routeConnectionsTimeLastSentToServerGauge.Reset()
	}
	if args.CollectRouteConnectionsTimeReceivedFromServer {
		routeConnectionsTimeLastReceivedFromServerGauge.Reset()
	}

	for _, route := range routes {
		routeGauge.WithLabelValues(route.Name)

		routeStatus, err := mysqlRouterClient.GetRouteStatus(route.Name)
		if err != nil {
			writeError(err)
			return
		}

		routeActiveConnectionsGauge.WithLabelValues(route.Name, router.Hostname).Set(float64(routeStatus.ActiveConnections))
		routeTotalConnectionsGauge.WithLabelValues(route.Name, router.Hostname).Set(float64(routeStatus.TotalConnections))
		routeBlockedHostsGauge.WithLabelValues(route.Name, router.Hostname).Set(float64(routeStatus.BlockedHosts))

		routeHealth, err := mysqlRouterClient.GetRouteHealth(route.Name)
		if err != nil {
			writeError(err)
			return
		}
		if routeHealth.IsAlive {
			routeHealthGauge.WithLabelValues(route.Name, router.Hostname).Set(float64(1))
		} else {
			routeHealthGauge.WithLabelValues(route.Name, router.Hostname).Set(float64(0))
		}

		routeDestinations, err := mysqlRouterClient.GetRouteDestinations(route.Name)
		if err != nil {
			writeError(err)
			return
		}
		for _, routeDestination := range routeDestinations {
			routeDestinationsGauge.WithLabelValues(route.Name, routeDestination.Address, strconv.Itoa(routeDestination.Port))
		}

		routeConnections, err := mysqlRouterClient.GetRouteConnections(route.Name)
		if err != nil {
			writeError(err)
			return
		}

		for _, routeConnection := range routeConnections {
			if args.CollectRouteConnectionsByteFromServer {
				routeConnectionsByteFromServerGauge.WithLabelValues(route.Name, router.Hostname, routeConnection.SourceAddress, routeConnection.DestinationAddress).Set(float64(routeConnection.BytesFromServer))
			}
			if args.CollectRouteConnectionsByteToServer {
				routeConnectionsByteToServerGauge.WithLabelValues(route.Name, router.Hostname, routeConnection.SourceAddress, routeConnection.DestinationAddress).Set(float64(routeConnection.BytesToServer))
			}
			if args.CollectRouteConnectionsTimeStarted {
				routeConnectionsTimeStartedGauge.WithLabelValues(route.Name, router.Hostname, routeConnection.SourceAddress, routeConnection.DestinationAddress).Set(float64(routeConnection.TimeStarted.Unix() * 1000)) // nolint
			}
			if args.CollectRouteConnectionsTimeConnectedToServer {
				routeConnectionsTimeConnectedToServerGauge.WithLabelValues(route.Name, router.Hostname, routeConnection.SourceAddress, routeConnection.DestinationAddress).Set(float64(routeConnection.TimeConnectedToServer.Unix() * 1000)) //nolint
			}
			if args.CollectRouteConnectionsTimeLastSentToServer {
				routeConnectionsTimeLastSentToServerGauge.WithLabelValues(route.Name, router.Hostname, routeConnection.SourceAddress, routeConnection.DestinationAddress).Set(float64(routeConnection.TimeLastSentToServer.Unix() * 1000)) // nolint
			}
			if args.CollectRouteConnectionsTimeReceivedFromServer {
				routeConnectionsTimeLastReceivedFromServerGauge.WithLabelValues(route.Name, router.Hostname, routeConnection.SourceAddress, routeConnection.DestinationAddress).Set(float64(routeConnection.TimeLastReceivedFromServer.Unix() * 1000)) // nolint
			}
		}
		lastRouteConnections = routeConnections
	}
	lastRoutes = routes
}

func writeError(err error) {
	_, _ = fmt.Fprintf(os.Stderr, "[mysqlrouter_exporter ERROR] %s\n", err.Error())
}

func main() {
	_, err := flags.Parse(&args)
	if err != nil {
		log.Fatalln(err)
	}
	if args.Version {
		fmt.Printf("version: %s commit: %s date: %s\n", version, commit, date)
		os.Exit(1)
	}

	mysqlRouterClient, err = initialClient()
	if err != nil {
		log.Fatalf("failed to create mysql router client. err: %s\n", err.Error())
	}

	collectInterval = time.Duration(args.CollectInterval) * time.Second
	recordMetrics()

	addr := fmt.Sprintf("0.0.0.0:%d", args.ListenPort)
	log.Printf("Start exporter on %s/metrics", addr)

	server := &http.Server{
		Addr:    addr,
		Handler: promhttp.Handler(),
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()
	log.Printf("Server started")

	<-done
	log.Printf("Server stopped")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer func() {
		cancel()
	}()

	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Server Shutdown Failed:%+v", err)
	}
	log.Printf("Server Exited Properly")
}
