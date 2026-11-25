package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
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
	"github.com/hoangndst/buddy-sandboxes/internal/controller"
	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
	"github.com/hoangndst/buddy-sandboxes/internal/gateway"
	"github.com/hoangndst/buddy-sandboxes/internal/gitbroker"
	"github.com/hoangndst/buddy-sandboxes/internal/identity"
	"github.com/hoangndst/buddy-sandboxes/internal/sessions"
	"github.com/hoangndst/buddy-sandboxes/internal/store"
)

var scheme = runtime.NewScheme()

type runtimeConfig struct {
	databaseURL      string
	encryptionKeys   string
	activeKeyVersion int
	hmac             []byte
}

func loadRuntimeConfig(getenv func(string) string) (runtimeConfig, error) {
	config := runtimeConfig{
		databaseURL:    getenv("BUDDY_DATABASE_URL"),
		encryptionKeys: getenv("BUDDY_CREDENTIAL_ENCRYPTION_KEYS_FILE"),
		hmac:           []byte(getenv("EGRESS_GATEWAY_HMAC_SECRET")),
	}
	activeVersion, err := strconv.Atoi(getenv("BUDDY_CREDENTIAL_ENCRYPTION_ACTIVE_VERSION"))
	if err != nil || activeVersion < 1 {
		return runtimeConfig{}, fmt.Errorf("BUDDY_CREDENTIAL_ENCRYPTION_ACTIVE_VERSION must be a positive integer")
	}
	config.activeKeyVersion = activeVersion
	if strings.TrimSpace(config.databaseURL) == "" {
		return runtimeConfig{}, fmt.Errorf("BUDDY_DATABASE_URL must be configured")
	}
	if config.encryptionKeys == "" {
		return runtimeConfig{}, fmt.Errorf("BUDDY_CREDENTIAL_ENCRYPTION_KEYS_FILE must be configured")
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
	if handled, err := runUtilityCommand(os.Args[1:]); handled {
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	var listenAddr string
	var drainTimeoutSec int

	flag.StringVar(&listenAddr, "listen-addr", ":8080", "Address on which the gateway listens for HTTP and WebSocket requests.")
	flag.IntVar(&drainTimeoutSec, "drain-timeout", 60, "Grace period in seconds to wait for active terminal sessions to complete on SIGTERM.")
	flag.Parse()
	runtimeCfg, err := loadRuntimeConfig(os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	hmacSecret := runtimeCfg.hmac
	database, err := store.Connect(context.Background(), runtimeCfg.databaseURL)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer database.Close()
	if err := database.RequireCurrentSchema(context.Background()); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	keySet, err := credentials.LoadKeySet(runtimeCfg.encryptionKeys, runtimeCfg.activeKeyVersion)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	identityStore := identity.NewStore(database.Pool)
	sessionStore := sessions.NewStore(database.Pool)
	providerCredentials := credentials.NewStore(database.Pool, keySet)
	gitCredentials := credentials.NewGitStore(database.Pool, keySet)
	brokerCapabilities := credentials.NewCapabilities(database.Pool)

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

	authenticator := gateway.NewDatabaseTokenAuthenticator(identityStore)

	credentialStore := gateway.NewCredentialSessionStore()
	sandboxState := gateway.NewMultiSandboxStateCache(k8sClient, identityStore)
	sandboxState.SetInvalidationHandler(func(namespace, sandboxName, creationID string) {
		_ = brokerCapabilities.RevokeSandbox(context.Background(), namespace, sandboxName, creationID)
		credentialStore.Revoke(namespace, sandboxName, creationID)
	})
	providerHandler, err := gateway.NewOpenAIProxy(gateway.OpenAIProxyConfig{
		HMACSecret:   hmacSecret,
		Store:        credentialStore,
		SandboxState: sandboxState,
	})
	if err != nil {
		log.Error(err, "unable to initialize provider broker")
		os.Exit(1)
	}
	anthropicHandler, err := gateway.NewAnthropicProxy(gateway.AnthropicProxyConfig{
		HMACSecret: hmacSecret, Store: credentialStore, SandboxState: sandboxState,
	})
	if err != nil {
		log.Error(err, "unable to initialize Anthropic provider broker")
		os.Exit(1)
	}
	credentialHandlers := gateway.NewCredentialHandlers(providerCredentials, k8sClient, identityStore)
	credentialHandlers.SetGitStore(gitCredentials)
	gitCredentialHandler := gitbroker.NewHandler(clientset, k8sClient, gitCredentials, identityStore, gitbroker.Audience)
	serverCfg := gateway.Config{
		ListenAddr:          listenAddr,
		DrainTimeout:        time.Duration(drainTimeoutSec) * time.Second,
		PropagationDelay:    5 * time.Second,
		Auth:                authenticator,
		Client:              k8sClient,
		KubeClient:          clientset,
		RemoteExecutor:      remoteExec,
		PortForwardDialer:   pfDialer,
		CredentialStore:     credentialStore,
		ProviderHandler:     providerHandler,
		AnthropicHandler:    anthropicHandler,
		BrokerState:         sandboxState,
		IdentityStore:       identityStore,
		SessionStore:        sessionStore,
		Credentials:         credentialHandlers,
		ProviderCredentials: providerCredentials,
		GitCredentials:      gitCredentials,
		GitBrokerHandler:    gitCredentialHandler,
		BrokerCapabilities:  brokerCapabilities,
		StorageReady:        database.Ready,
	}

	srv := gateway.NewServer(serverCfg)

	stopCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sandboxState.Run(stopCtx)
	go controller.RunGitBindingSweeper(stopCtx, gitCredentials, k8sClient)

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
