package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	ver                 string = "0.12"
	loopInterval        int    = 300
	negStatusAnnotation string = "cloud.google.com/neg-status"
)

var (
	listenAddress = kingpin.Flag("web.listen-address", "Address to listen on for web interface and telemetry.").Default(":8080").String()
	namespace     = kingpin.Flag("namespace", "Namespace name.").Envar("NAMESPACE").Default("").String()
	verbose       = kingpin.Flag("verbose", "Verbose mode.").Short('v').Bool()
)

var (
	kubeServiceGcpBackends = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kube_service_gcp_backends",
		Help: "Kubernetes Service GCP backends",
	},
		[]string{"service_backend", "gcp_backend"})
	errorsCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kube_service_gcp_backends_errors_total",
		Help: "Kubernetes Service GCP backends errors",
	})
)

type NegStatusAnnotation struct {
	NetworkEndpointGroups map[string]string `json:"network_endpoint_groups"`
}

func performRecordMetrics(ctx context.Context, clientset *kubernetes.Clientset) {
	slog.Debug("Fetching Services")
	services, err := clientset.CoreV1().Services(*namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		slog.Error("List Services failed", "error", err)
		errorsCounter.Inc()
		return
	}

	for _, service := range services.Items {
		for annotationKey, annotationValue := range service.Annotations {
			if annotationKey == negStatusAnnotation {
				var negStatusAnnotation NegStatusAnnotation
				err := json.Unmarshal([]byte(annotationValue), &negStatusAnnotation)
				if err != nil {
					slog.Error("Unmarshalling NEG status annotation failed", "error", err)
					errorsCounter.Inc()
					continue
				}

				for port, gcpBackend := range negStatusAnnotation.NetworkEndpointGroups {
					serviceBackend := fmt.Sprintf("%s_%s_%s", service.Namespace, service.Name, port)

					slog.Debug("Set metric", "service backend", serviceBackend, "gcp backend", gcpBackend)
					kubeServiceGcpBackends.WithLabelValues(serviceBackend, gcpBackend).Set(1)
				}
			}
		}
	}
}

func recordMetrics(ctx context.Context, clientset *kubernetes.Clientset) {
	slog.Info("Get services", "namespace", *namespace)

	ticker := time.NewTicker(time.Second * time.Duration(loopInterval))
	defer ticker.Stop()

	performRecordMetrics(ctx, clientset)

	for {
		select {
		case <-ctx.Done():
			slog.Info("Record metrics shutting down...")
			return
		case <-ticker.C:
			performRecordMetrics(ctx, clientset)
		}
	}
}

// handleHealthz responds with "OK" indicating the application is running.
func handleHealthz(w http.ResponseWriter, req *http.Request) {
	fmt.Fprintf(w, "OK\n")
}

// startHTTPServer starts the HTTP server to handle health and metrics endpoints.
func startHTTPServer(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.Handle("/metrics", promhttp.Handler())

	server := &http.Server{
		Addr:    *listenAddress,
		Handler: mux,
	}

	// Shutdown the server gracefully when context is done
	go func() {
		<-ctx.Done()
		slog.Info("Shutting down HTTP server...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("Error shutting down HTTP server", "error", err)
		}
	}()

	slog.Info("Starting HTTP server", "address", *listenAddress)

	return server.ListenAndServe()
}

// Initialize Kubernetes client
func initKubernetesClient() (*kubernetes.Clientset, error) {
	config, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("Cannot get Kubernetes config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("Cannot create Kubernetes clientset: %w", err)
	}

	return clientset, nil
}

func main() {
	var loggingLevel = new(slog.LevelVar)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: loggingLevel}))
	slog.SetDefault(logger)

	kingpin.Version(ver)
	kingpin.Parse()

	if *verbose {
		loggingLevel.Set(slog.LevelDebug)
	}

	slog.Info("Program started", "version", ver)

	errorsCounter.Add(0)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	clientset, err := initKubernetesClient()
	if err != nil {
		slog.Error("Failed to initialize Kubernetes client", "error", err)
		os.Exit(1)
	}

	go recordMetrics(ctx, clientset)

	// Start the HTTP server
	if err := startHTTPServer(ctx); err != nil && err != http.ErrServerClosed {
		slog.Error("HTTP server encountered an error", "error", err)
		os.Exit(1)
	}

	slog.Info("Program gracefully stopped")
}
