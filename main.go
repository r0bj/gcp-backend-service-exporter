package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
	"log/slog"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/alecthomas/kingpin/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	ver                 string = "0.11"
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
)

type NegStatusAnnotation struct {
	NetworkEndpointGroups map[string]string `json:"network_endpoint_groups"`
}

func recordMetrics() {
	ctx := context.Background()
	config := ctrl.GetConfigOrDie()
	clientset := kubernetes.NewForConfigOrDie(config)

	slog.Info("Get services", "namespace", *namespace)

	ticker := time.NewTicker(time.Second * time.Duration(loopInterval))
	for ; true; <-ticker.C {
		slog.Debug("Fetching Services")
		services, err := clientset.CoreV1().Services(*namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			slog.Error("List Services failed", "error", err)
			continue
		}

		for _, service := range services.Items {
			for annotationKey, annotationValue := range service.Annotations {
				if annotationKey == negStatusAnnotation {
					var negStatusAnnotation NegStatusAnnotation
					err := json.Unmarshal([]byte(annotationValue), &negStatusAnnotation)
					if err != nil {
						slog.Error("Unmarshalling NEG status annotation failed", "error", err)
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

	go recordMetrics()

	http.Handle("/metrics", promhttp.Handler())

	slog.Info("Listening", "address", *listenAddress)
	err := http.ListenAndServe(*listenAddress, nil)
	if err != nil {
		slog.Error("Listening", "error", err)
	}
}
