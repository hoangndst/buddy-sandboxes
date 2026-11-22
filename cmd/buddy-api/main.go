package main

import (
	"context"
	"flag"
	"fmt"
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
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"github.com/hoangndst/buddy-sandboxes/internal/gateway"
)

var scheme = runtime.NewScheme()

type runtimeConfig struct {
	token     string
	namespace string
	hmac      []byte
}

func loadRuntimeConfig(getenv func(string) string) (runtimeConfig, error) {
	config := runtimeConfig{token: getenv("BUDDY_API_TOKEN"), namespace: getenv("BUDDY_API_NAMESPACE"), hmac: []byte(getenv("EGRESS_GATEWAY_HMAC_SECRET"))}
	if config.token == "" || config.token == "dev-token-secret" || len(config.token) < 32 {
		return runtimeConfig{}, fmt.Errorf("BUDDY_API_TOKEN must contain at least 32 bytes and may not use the development token")
	}
	if config.namespace == "" {
		return runtimeConfig{}, fmt.Errorf("BUDDY_API_NAMESPACE must be configured")
	}
	if len(config.hmac) < 32 || string(config.hmac) == "REPLACE_WITH_HMAC_SECRET_KEY_32BYTES" {
		return runtimeConfig{}, fmt.Errorf("EGRESS_GATEWAY_HMAC_SECRET must contain at least 32 bytes")
	}
	return config, nil
}

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
	flag.StringVar(&defaultToken, "default-token", "", "Pre-shared bearer token for authentication.")
	flag.StringVar(&defaultNamespace, "default-namespace", "", "Kubernetes namespace mapped to default token.")
	flag.Parse()
	runtimeCfg, err := loadRuntimeConfig(func(key string) string {
		switch key {
		case "BUDDY_API_TOKEN":
			if defaultToken != "" {
				return defaultToken
			}
		case "BUDDY_API_NAMESPACE":
			if defaultNamespace != "" {
				return defaultNamespace
			}
		}
		return os.Getenv(key)
	})
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defaultToken, defaultNamespace = runtimeCfg.token, runtimeCfg.namespace
	hmacSecret := runtimeCfg.hmac

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log.WithName("main")

	log.Info("loading Kubernetes configuration")
	kubeConfig, err := ctrl.GetConfig()
	if err != nil {
		log.Error(err, "unable to get kubeconfig")
		os.Exit(1)
	}

	k8sClient, err := client.NewWithWatch(kubeConfig, client.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "unable to initialize kubernetes client")
		os.Exit(1)
	}
	log.Info("Kubernetes API client initialized")

	clientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		log.Error(err, "unable to initialize kubernetes clientset")
		os.Exit(1)
	}
	log.Info("Kubernetes clientset initialized")

	remoteExec, err := gateway.NewKubeRemoteExecutor(kubeConfig)
	if err != nil {
		log.Error(err, "unable to initialize kubernetes remote executor")
		os.Exit(1)
	}
	log.Info("remote executor initialized")

	pfDialer := gateway.NewKubePortForwardDialer(kubeConfig, clientset)

	tokenMap := map[string]string{
		defaultToken: defaultNamespace,
	}
	authenticator := gateway.NewStaticTokenAuthenticator(tokenMap)

	credentialStore := gateway.NewCredentialSessionStore()
	sandboxState := gateway.NewSandboxStateCache(k8sClient, defaultNamespace)
	sandboxState.SetInvalidationHandler(credentialStore.Revoke)
	providerHandler, err := gateway.NewOpenAIProxy(gateway.OpenAIProxyConfig{
		HMACSecret:   hmacSecret,
		Store:        credentialStore,
		SandboxState: sandboxState,
	})
	if err != nil {
		log.Error(err, "unable to initialize provider broker")
		os.Exit(1)
	}
	serverCfg := gateway.Config{
		ListenAddr:        listenAddr,
		DrainTimeout:      time.Duration(drainTimeoutSec) * time.Second,
		PropagationDelay:  5 * time.Second,
		Auth:              authenticator,
		Client:            k8sClient,
		KubeClient:        clientset,
		RemoteExecutor:    remoteExec,
		PortForwardDialer: pfDialer,
		CredentialStore:   credentialStore,
		ProviderHandler:   providerHandler,
		BrokerState:       sandboxState,
	}

	srv := gateway.NewServer(serverCfg)

	stopCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sandboxState.Run(stopCtx)

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
