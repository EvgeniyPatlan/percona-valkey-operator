/*
Copyright Percona LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package valkeynode

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// ptrTo returns a pointer to v.
func ptrTo[T any](v T) *T { return &v }

// mustQuantity parses a resource.Quantity, panicking on a bad literal. Used only
// for compile-time-constant defaults (never user input).
func mustQuantity(s string) resource.Quantity {
	return resource.MustParse(s)
}

// setCondition upserts a condition on the node status with ObservedGeneration
// stamped to the current generation.
func setCondition(node *valkeyv1alpha1.ValkeyNode, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})
}

// conditionTrue reports whether the named condition is present and True.
func conditionTrue(node *valkeyv1alpha1.ValkeyNode, condType string) bool {
	return meta.IsStatusConditionTrue(node.Status.Conditions, condType)
}

// boolToStatus maps a bool to a metav1.ConditionStatus.
func boolToStatus(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// isPodReady reports whether the pod has a Ready condition set True.
func isPodReady(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
