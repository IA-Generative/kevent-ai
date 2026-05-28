package lifecycle

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"
)

const (
	serviceAccountTokenPath     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	serviceAccountCAPath        = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	serviceAccountNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	kubernetesAPIURL            = "https://kubernetes.default.svc"
	deletionCostAnnotation      = "controller.kubernetes.io/pod-deletion-cost"

	// CostBusy is set while the relay is processing at least one job, making
	// Kubernetes prefer other (idle) pods when the autoscaler scales down.
	CostBusy = 1000
	// CostIdle signals that this pod has no active jobs and should be
	// preferred for deletion during scale-down.
	CostIdle = 0
)

// PodAnnotator patches controller.kubernetes.io/pod-deletion-cost on the
// relay's own pod via the in-cluster Kubernetes API.
//
// This annotation is evaluated by the ReplicaSet controller when selecting
// which pod to terminate during scale-down: lower cost = deleted first.
// Neither Knative Serving nor KServe reconcile individual pod annotations,
// so patches applied here persist until the pod is deleted.
type PodAnnotator struct {
	namespace string
	podName   string
	client    *http.Client
}

// New returns a PodAnnotator using the in-cluster service account. Returns nil
// when running outside Kubernetes (no service account token), in which case
// annotation management is disabled.
//
// Pod name is read from /etc/hostname (set by Kubernetes for every pod).
// Namespace is read from the service account namespace file — no Downward API
// env vars required, which avoids KServe ServingRuntime fieldRef restrictions.
func New() *PodAnnotator {
	if _, err := os.Stat(serviceAccountTokenPath); err != nil {
		slog.Info("pod-deletion-cost management disabled (no service account token)")
		return nil
	}

	podName, err := os.Hostname()
	if err != nil || podName == "" {
		slog.Info("pod-deletion-cost management disabled (cannot read hostname)", "error", err)
		return nil
	}

	nsBytes, err := os.ReadFile(serviceAccountNamespacePath)
	if err != nil {
		slog.Info("pod-deletion-cost management disabled (cannot read namespace)", "error", err)
		return nil
	}
	namespace := string(bytes.TrimSpace(nsBytes))
	if namespace == "" {
		slog.Info("pod-deletion-cost management disabled (empty namespace)")
		return nil
	}

	caCert, err := os.ReadFile(serviceAccountCAPath)
	if err != nil {
		slog.Warn("pod-deletion-cost management disabled: cannot read cluster CA", "error", err)
		return nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		slog.Warn("pod-deletion-cost management disabled: cannot parse cluster CA")
		return nil
	}

	slog.Info("pod-deletion-cost management enabled", "pod", podName, "namespace", namespace)
	return &PodAnnotator{
		namespace: namespace,
		podName:   podName,
		client: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool},
			},
		},
	}
}

// SetDeletionCost patches the pod's deletion-cost annotation.
// Use CostBusy when the pod is processing a job, CostIdle when idle.
func (a *PodAnnotator) SetDeletionCost(ctx context.Context, cost int) error {
	token, err := os.ReadFile(serviceAccountTokenPath)
	if err != nil {
		return fmt.Errorf("reading service account token: %w", err)
	}

	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{
				deletionCostAnnotation: fmt.Sprintf("%d", cost),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("marshalling patch: %w", err)
	}

	url := fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s",
		kubernetesAPIURL, a.namespace, a.podName)

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Header.Set("Authorization", "Bearer "+string(bytes.TrimSpace(token)))

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}
