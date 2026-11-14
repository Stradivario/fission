// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
)

// CreateFailureReason is the Event reason the executor records when it
// cannot create a function's Kubernetes objects.
const CreateFailureReason = "FailedCreate"

const maxEventMessage = 1024

// RecordCreateFailure writes a Warning Event on the Function when the
// executor cannot create one of its Kubernetes objects (the Service, the
// Deployment, ...). Until now such a failure only reached the executor's
// own log: e.g. "services ... is forbidden: exceeded quota" left a function
// that never started, and nothing outside the executor knew why. As an
// Event it shows in `kubectl describe function` and reaches anything that
// watches Events (Lambforge turns quota Events into project notifications).
//
// One Event per function and object kind: a repeated failure (the
// executor retries on every request) updates its count and last time
// instead of creating a new Event each time. Never fails the caller - a
// missing permission or a conflict is only logged.
func RecordCreateFailure(
	ctx context.Context,
	client kubernetes.Interface,
	logger logr.Logger,
	fn *fv1.Function,
	what string,
	cause error,
) {
	if client == nil || fn == nil || cause == nil {
		return
	}
	message := fmt.Sprintf("could not create the function's %s: %v", what, cause)
	if len(message) > maxEventMessage {
		message = message[:maxEventMessage]
	}
	name := createFailureEventName(fn, what)
	now := metav1.NewTime(time.Now())
	events := client.CoreV1().Events(fn.Namespace)

	existing, err := events.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		existing.Count++
		existing.LastTimestamp = now
		existing.Message = message
		if _, err := events.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
			logger.V(1).Info("could not update create-failure event", "event", name, "error", err.Error())
		}
		return
	}
	if !k8serrors.IsNotFound(err) {
		logger.V(1).Info("could not read create-failure event", "event", name, "error", err.Error())
		return
	}
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: fn.Namespace},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: "fission.io/v1",
			Kind:       "Function",
			Name:       fn.Name,
			Namespace:  fn.Namespace,
			UID:        fn.UID,
		},
		Reason:              CreateFailureReason,
		Message:             message,
		Type:                corev1.EventTypeWarning,
		Source:              corev1.EventSource{Component: "fission-executor"},
		ReportingController: "fission.io/executor",
		FirstTimestamp:      now,
		LastTimestamp:       now,
		Count:               1,
	}
	if _, err := events.Create(ctx, event, metav1.CreateOptions{}); err != nil {
		logger.V(1).Info("could not record create-failure event", "event", name, "error", err.Error())
	}
}

// createFailureEventName is stable per function (name + uid) and object
// kind, so retries land on the same Event.
func createFailureEventName(fn *fv1.Function, what string) string {
	sum := sha1.Sum([]byte(string(fn.UID) + "/" + what))
	suffix := hex.EncodeToString(sum[:])[:10]
	base := fn.Name
	if len(base) > 200 {
		base = base[:200]
	}
	return fmt.Sprintf("%s.create-failed.%s", base, suffix)
}
