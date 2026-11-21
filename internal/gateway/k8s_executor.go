package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

// KubeRemoteExecutor executes commands on Kubernetes pods using SPDY/WebSocket remotecommand transport.
type KubeRemoteExecutor struct {
	config     *rest.Config
	kubeClient kubernetes.Interface
}

// NewKubeRemoteExecutor creates a new KubeRemoteExecutor.
func NewKubeRemoteExecutor(cfg *rest.Config) (*KubeRemoteExecutor, error) {
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}
	return &KubeRemoteExecutor{
		config:     cfg,
		kubeClient: clientset,
	}, nil
}

// StreamExec executes a command inside the target pod container.
func (e *KubeRemoteExecutor) StreamExec(
	ctx context.Context,
	namespace, podName string,
	cmd []string,
	tty bool,
	stdin io.Reader,
	stdout, stderr io.Writer,
	resizeChan <-chan apiv1.ResizeMessage,
) (int, error) {
	req := e.kubeClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	req.VersionedParams(&corev1.PodExecOptions{
		Container: "sandbox",
		Command:   cmd,
		Stdin:     stdin != nil,
		Stdout:    stdout != nil,
		Stderr:    stderr != nil,
		TTY:       tty,
	}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(e.config, http.MethodPost, req.URL())
	if err != nil {
		return 1, fmt.Errorf("failed to initialize spdy executor: %w", err)
	}

	var termQueue remotecommand.TerminalSizeQueue
	if tty && resizeChan != nil {
		termQueue = &kubeTerminalSizeQueue{resizeChan: resizeChan}
	}

	streamOpts := remotecommand.StreamOptions{
		Stdin:             stdin,
		Stdout:            stdout,
		Stderr:            stderr,
		Tty:               tty,
		TerminalSizeQueue: termQueue,
	}

	err = exec.StreamWithContext(ctx, streamOpts)
	if err != nil {
		if exitErr, ok := err.(interface{ ExitStatus() int }); ok {
			return exitErr.ExitStatus(), nil
		}
		return 1, err
	}

	return 0, nil
}

type kubeTerminalSizeQueue struct {
	resizeChan <-chan apiv1.ResizeMessage
}

func (q *kubeTerminalSizeQueue) Next() *remotecommand.TerminalSize {
	msg, ok := <-q.resizeChan
	if !ok {
		return nil
	}
	cols := msg.Cols
	if cols == 0 && msg.Width > 0 {
		cols = int(msg.Width)
	}
	rows := msg.Rows
	if rows == 0 && msg.Height > 0 {
		rows = int(msg.Height)
	}
	return &remotecommand.TerminalSize{
		Width:  uint16(cols),
		Height: uint16(rows),
	}
}
