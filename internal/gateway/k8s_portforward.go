package gateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// NewKubePortForwardDialer constructs a PortForwardDialer that tunnels through the Kubernetes API server (DEC-005).
func NewKubePortForwardDialer(cfg *rest.Config, kubeClient kubernetes.Interface) PortForwardDialer {
	return func(ctx context.Context, namespace, podName string, targetPort int) (net.Conn, error) {
		req := kubeClient.CoreV1().RESTClient().Post().
			Resource("pods").
			Namespace(namespace).
			Name(podName).
			SubResource("portforward")

		roundTripper, upgrader, err := spdy.RoundTripperFor(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize spdy roundtripper: %w", err)
		}

		dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, req.URL())
		return dialKubePortForward(ctx, dialer, targetPort)
	}
}

func dialKubePortForward(ctx context.Context, dialer httpstream.Dialer, targetPort int) (net.Conn, error) {
	stopChan := make(chan struct{})
	readyChan := make(chan struct{})

	ports := []string{fmt.Sprintf("0:%d", targetPort)}
	pf, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, ports, stopChan, readyChan, io.Discard, io.Discard)
	if err != nil {
		close(stopChan)
		return nil, fmt.Errorf("failed to create portforwarder: %w", err)
	}

	go func() {
		_ = pf.ForwardPorts()
	}()

	select {
	case <-readyChan:
	case <-ctx.Done():
		close(stopChan)
		return nil, ctx.Err()
	case <-time.After(5 * time.Second):
		close(stopChan)
		return nil, fmt.Errorf("timed out waiting for kubernetes port forward")
	}

	forwardedPorts, err := pf.GetPorts()
	if err != nil || len(forwardedPorts) == 0 {
		close(stopChan)
		return nil, fmt.Errorf("failed to get forwarded ports: %w", err)
	}

	localConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", forwardedPorts[0].Local), 3*time.Second)
	if err != nil {
		close(stopChan)
		return nil, fmt.Errorf("failed to connect to local forwarded port: %w", err)
	}

	return &wrappedPortForwardConn{
		Conn:     localConn,
		stopChan: stopChan,
	}, nil
}

type wrappedPortForwardConn struct {
	net.Conn
	stopChan chan struct{}
	once     sync.Once
}

func (w *wrappedPortForwardConn) Close() error {
	w.once.Do(func() {
		close(w.stopChan)
	})
	return w.Conn.Close()
}
