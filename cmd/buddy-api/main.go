package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"github.com/hoangndst/buddy-sandboxes/internal/gateway"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(sandboxesv1alpha1.AddToScheme(scheme))
}

func main() {
	var listenAddr string
	var drainTimeoutSec int
	var defaultToken string
	var defaultNamespace string

	flag.StringVar(&listenAddr, "listen-addr", ":8080", "Address on which the gateway listens for HTTP and WebSocket requests.")
	flag.IntVar(&drainTimeoutSec, "drain-timeout", 60, "Grace period in seconds to wait for active terminal sessions to complete on SIGTERM.")
	flag.StringVar(&defaultToken, "default-token", "dev-token-secret", "Pre-shared bearer token for authentication.")
	flag.StringVar(&defaultNamespace, "default-namespace", "default", "Kubernetes namespace mapped to default token.")
	flag.Parse()

	ctrl.SetLogger(ctrl.Log.WithName("buddy-api"))
	log := ctrl.Log.WithName("main")

	kubeConfig, err := ctrl.GetConfig()
	if err != nil {
		log.Error(err, "unable to get kubeconfig")
		os.Exit(1)
	}

	k8sClient, err := client.New(kubeConfig, client.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "unable to initialize kubernetes client")
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		log.Error(err, "unable to initialize kubernetes clientset")
		os.Exit(1)
	}

	remoteExec, err := gateway.NewKubeRemoteExecutor(kubeConfig)
	if err != nil {
		log.Error(err, "unable to initialize kubernetes remote executor")
		os.Exit(1)
	}

	pfDialer := gateway.NewKubePortForwardDialer(kubeConfig, clientset)

	tokenMap := map[string]string{
		defaultToken: defaultNamespace,
	}
	authenticator := gateway.NewStaticTokenAuthenticator(tokenMap)

	serverCfg := gateway.Config{
		ListenAddr:        listenAddr,
		DrainTimeout:      time.Duration(drainTimeoutSec) * time.Second,
		PropagationDelay:  5 * time.Second,
		Auth:              authenticator,
		Client:            k8sClient,
		KubeClient:        clientset,
		RemoteExecutor:    remoteExec,
		PortForwardDialer: pfDialer,
	}

	srv := gateway.NewServer(serverCfg)

	stopCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("starting buddy-api gateway daemon", "listenAddr", listenAddr)
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			log.Error(err, "gateway server terminated with error")
			os.Exit(1)
		}
	}()

	<-stopCtx.Done()
	log.Info("received termination signal, initiating two-phase graceful drain (DEC-004)...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Duration(drainTimeoutSec+15)*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error(err, "error encountered during graceful shutdown")
	} else {
		log.Info("gateway server cleanly terminated")
	}
}
