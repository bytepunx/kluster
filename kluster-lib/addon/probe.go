package addon

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	svProbeNamespace  = "default"
	svProbeName       = "kluster-svid-probe"
	svProbeMountPath  = "/run/spiffe.io"
	svProbeSocketPath = svProbeMountPath + "/spire-agent.sock"
)

// ProbeSVID deploys a short-lived pod using the same SPIRE agent image as the
// running agent. The pod requests an X.509 SVID via the SPIFFE workload API
// socket (mounted via csi.spiffe.io) and exits. We wait for the pod to succeed
// and then verify the SPIFFE ID in the log output.
//
// This confirms end-to-end SPIRE functionality: the agent is attesting workloads
// and the server is issuing SVIDs with the correct trust domain.
func ProbeSVID(ctx context.Context, h ClusterHandle, trustDomain string) error {
	agentImage, err := spireAgentImage(ctx, h)
	if err != nil {
		return err
	}

	defer func() {
		_ = h.K8sClient.CoreV1().Pods(svProbeNamespace).Delete(
			context.Background(), svProbeName, metav1.DeleteOptions{},
		)
	}()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svProbeName,
			Namespace: svProbeNamespace,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: "default",
			// OnFailure: retry until SPIRE has processed the registration entry.
			RestartPolicy: corev1.RestartPolicyOnFailure,
			Containers: []corev1.Container{{
				Name:  "probe",
				Image: agentImage,
				Command: []string{
					"/opt/spire/bin/spire-agent", "api", "fetch", "x509",
					"-socketPath", svProbeSocketPath,
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "spiffe",
					MountPath: svProbeMountPath,
					ReadOnly:  true,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "spiffe",
				VolumeSource: corev1.VolumeSource{
					CSI: &corev1.CSIVolumeSource{
						Driver:   "csi.spiffe.io",
						ReadOnly: boolPtr(true),
					},
				},
			}},
		},
	}

	if _, err := h.K8sClient.CoreV1().Pods(svProbeNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create probe pod: %w", err)
	}

	if err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			p, err := h.K8sClient.CoreV1().Pods(svProbeNamespace).Get(ctx, svProbeName, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			switch p.Status.Phase {
			case corev1.PodSucceeded:
				return true, nil
			case corev1.PodFailed:
				return false, fmt.Errorf("probe pod entered Failed phase (SVID issuance failed)")
			}
			return false, nil
		},
	); err != nil {
		return fmt.Errorf("SVID fetch did not complete: %w", err)
	}

	output, err := readPodLogs(ctx, h, svProbeNamespace, svProbeName, "probe")
	if err != nil {
		return fmt.Errorf("read probe logs: %w", err)
	}

	return verifySpiffeIDOutput(output, trustDomain)
}

// spireAgentImage returns the container image used by the running SPIRE agent
// DaemonSet so the probe pod runs the same binary version.
func spireAgentImage(ctx context.Context, h ClusterHandle) (string, error) {
	ds, err := h.K8sClient.AppsV1().DaemonSets(spireNamespace).Get(ctx, "spire-agent", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get spire-agent daemonset: %w", err)
	}
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name == "spire-agent" {
			return c.Image, nil
		}
	}
	return "", fmt.Errorf("spire-agent container not found in DaemonSet")
}

func readPodLogs(ctx context.Context, h ClusterHandle, namespace, pod, container string) (string, error) {
	req := h.K8sClient.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// verifySpiffeIDOutput parses the text output of `spire-agent api fetch x509`
// and verifies at least one SVID has a SPIFFE ID under the expected trust domain.
//
// Expected line format: "SPIFFE ID:\t\tspiffe://<trustDomain>/..."
func verifySpiffeIDOutput(output, trustDomain string) error {
	prefix := "spiffe://" + trustDomain + "/"
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "SPIFFE ID:") {
			continue
		}
		spiffeID := strings.TrimSpace(strings.TrimPrefix(line, "SPIFFE ID:"))
		if strings.HasPrefix(spiffeID, prefix) {
			return nil
		}
		return fmt.Errorf("unexpected SPIFFE ID %q: want trust domain %q", spiffeID, trustDomain)
	}
	return fmt.Errorf("no SPIFFE ID line found in fetch output:\n%s", output)
}
