/*
Copyright 2022 The Kubernetes Authors.

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

package utils

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "sigs.k8s.io/cluster-api-provider-cloudstack/api/v1beta3"
)

// getMachineSetFromCAPIMachine attempts to fetch a MachineSet from CAPI machine owner reference.

// fetchOwnerRef simply searches a list of OwnerReference objects for a given kind.
func fetchOwnerRef(refList []metav1.OwnerReference, kind string) *metav1.OwnerReference {
	for _, ref := range refList {
		if ref.Kind == kind {
			return &ref
		}
	}

	return nil
}

// GetManagementOwnerRef returns the reference object pointing to the CAPI machine's manager.
func GetManagementOwnerRef(capiMachine *clusterv1.Machine) *metav1.OwnerReference {
	if util.IsControlPlaneMachine(capiMachine) {
		return fetchOwnerRef(capiMachine.OwnerReferences, "KubeadmControlPlane")
	} else if ref := fetchOwnerRef(capiMachine.OwnerReferences, "EtcdadmCluster"); ref != nil {
		return ref
	}

	return fetchOwnerRef(capiMachine.OwnerReferences, "MachineSet")
}

// GetOwnerOfKind returns the Cluster object owning the current resource of passed kind.
func GetOwnerOfKind(ctx context.Context, c client.Client, owned client.Object, owner client.Object) error {
	gvks, _, err := c.Scheme().ObjectKinds(owner)
	if err != nil {
		return errors.Wrapf(err, "finding owner kind for %s/%s", owned.GetName(), owned.GetNamespace())
	} else if len(gvks) != 1 {
		return errors.Errorf(
			"found more than one GVK for owner when finding owner kind for %s/%s", owned.GetName(), owned.GetNamespace())
	}
	gvk := gvks[0]

	for _, ref := range owned.GetOwnerReferences() {
		if ref.Kind != gvk.Kind {
			continue
		}
		key := client.ObjectKey{Name: ref.Name, Namespace: owned.GetNamespace()}
		if err := c.Get(ctx, key, owner); err != nil {
			return errors.Wrapf(err, "finding owner of kind %s in namespace %s", gvk.Kind, owned.GetNamespace())
		}

		return nil
	}

	return errors.Errorf("couldn't find owner of kind %s in namespace %s", gvk.Kind, owned.GetNamespace())
}

func ContainsNoMatchSubstring(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "no match")
}

func ContainsAlreadyExistsSubstring(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "already exists")
}

// GetOwnerClusterName returns the Cluster name of the cluster owning the current resource.
func GetOwnerClusterName(obj metav1.ObjectMeta) (string, error) {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind != "Cluster" {
			continue
		}
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			return "", errors.WithStack(err)
		}
		if gv.Group == clusterv1.GroupVersion.Group {
			return ref.Name, nil
		}
	}

	return "", errors.New("failed to get owner Cluster name")
}

// CloudStackClusterToCloudStackMachines is a handler.ToRequestsFunc to be used to enqueue requests for reconciliation
// of CloudStackMachines.
func CloudStackClusterToCloudStackMachines(c client.Client, log logr.Logger) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		csCluster, ok := o.(*infrav1.CloudStackCluster)
		if !ok {
			log.Error(fmt.Errorf("expected a CloudStackCluster but got a %T", o), "Error in CloudStackClusterToCloudStackMachines")

			return nil
		}

		log := log.WithValues("objectMapper", "cloudstackClusterToCloudStackMachine", "cluster", klog.KRef(csCluster.Namespace, csCluster.Name))

		// Don't handle deleted CloudStackClusters
		if !csCluster.ObjectMeta.DeletionTimestamp.IsZero() {
			log.V(4).Info("CloudStackCluster has a deletion timestamp, skipping mapping.")

			return nil
		}

		clusterName, err := GetOwnerClusterName(csCluster.ObjectMeta)
		if err != nil {
			log.Error(err, "Failed to get owning cluster, skipping mapping.")

			return nil
		}

		machineList := &clusterv1.MachineList{}
		// list all the requested objects within the cluster namespace with the cluster name label
		if err := c.List(ctx, machineList, client.InNamespace(csCluster.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: clusterName}); err != nil {
			log.Error(err, "Failed to get owned Machines, skipping mapping.")

			return nil
		}

		results := make([]ctrl.Request, 0, len(machineList.Items))
		for _, machine := range machineList.Items {
			m := machine
			log.WithValues("machine", klog.KObj(&m))
			if m.Spec.InfrastructureRef.GroupVersionKind().Kind != "CloudStackMachine" {
				log.V(4).Info("Machine has an InfrastructureRef for a different type, will not add to reconciliation request.")

				continue
			}
			if m.Spec.InfrastructureRef.Name == "" {
				log.V(4).Info("Machine has an InfrastructureRef with an empty name, will not add to reconciliation request.")

				continue
			}
			log.WithValues("cloudStackMachine", klog.KRef(m.Spec.InfrastructureRef.Namespace, m.Spec.InfrastructureRef.Name))
			log.V(4).Info("Adding CloudStackMachine to reconciliation request.")
			results = append(results, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: m.Namespace, Name: m.Spec.InfrastructureRef.Name}})
		}

		return results
	}
}

// CloudStackClusterToCloudStackIsolatedNetworks is a handler.ToRequestsFunc to be used to enqueue requests for reconciliation
// of CloudStackIsolatedNetworks.
func CloudStackClusterToCloudStackIsolatedNetworks(c client.Client, obj client.ObjectList, scheme *runtime.Scheme, log logr.Logger) (handler.MapFunc, error) {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return nil, errors.Wrap(err, "failed to find GVK for CloudStackIsolatedNetwork")
	}

	return func(ctx context.Context, o client.Object) []reconcile.Request {
		csCluster, ok := o.(*infrav1.CloudStackCluster)
		if !ok {
			log.Error(fmt.Errorf("expected a CloudStackCluster but got a %T", o), "Error in CloudStackClusterToCloudStackIsolatedNetworks")

			return nil
		}

		log = log.WithValues("objectMapper", "cloudstackClusterToCloudStackIsolatedNetworks", "cluster", klog.KRef(csCluster.Namespace, csCluster.Name))

		// Don't handle deleted CloudStackClusters
		if !csCluster.ObjectMeta.DeletionTimestamp.IsZero() {
			log.V(4).Info("CloudStackCluster has a deletion timestamp, skipping mapping.")

			return nil
		}

		clusterName, err := GetOwnerClusterName(csCluster.ObjectMeta)
		if err != nil {
			log.Error(err, "Failed to get owning cluster, skipping mapping.")

			return nil
		}

		isonetList := &infrav1.CloudStackIsolatedNetworkList{}
		isonetList.SetGroupVersionKind(gvk)

		// list all the requested objects within the cluster namespace with the cluster name label
		if err := c.List(ctx, isonetList, client.InNamespace(csCluster.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: clusterName}); err != nil {
			return nil
		}

		var results []reconcile.Request
		for _, isonet := range isonetList.Items {
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: isonet.GetNamespace(),
					Name:      isonet.GetName(),
				},
			}
			results = append(results, req)
		}

		return results
	}, nil
}

// CloudStackIsolatedNetworkToControlPlaneCloudStackMachines is a handler.ToRequestsFunc to be used to enqueue requests for reconciliation
// of CloudStackMachines that are part of the control plane.
func CloudStackIsolatedNetworkToControlPlaneCloudStackMachines(c client.Client, log logr.Logger) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		csIsoNet, ok := o.(*infrav1.CloudStackIsolatedNetwork)
		if !ok {
			log.Error(fmt.Errorf("expected a CloudStackIsolatedNetwork but got a %T", o), "Error in CloudStackIsolatedNetworkToControlPlaneCloudStackMachines")

			return nil
		}

		log := log.WithValues("objectMapper", "cloudStackIsolatedNetworkToControlPlaneCloudStackMachines", "isonet", klog.KRef(csIsoNet.Namespace, csIsoNet.Name))

		// Don't handle deleted CloudStackIsolatedNetworks
		if !csIsoNet.ObjectMeta.DeletionTimestamp.IsZero() {
			log.V(4).Info("CloudStackIsolatedNetwork has a deletion timestamp, skipping mapping.")

			return nil
		}

		clusterName, ok := csIsoNet.GetLabels()[clusterv1.ClusterNameLabel]
		if !ok {
			log.Error(errors.New("failed to find cluster name label"), "CloudStackIsolatedNetwork is missing cluster name label or cluster does not exist, skipping mapping.")
		}

		machineList := &clusterv1.MachineList{}
		// list all the requested objects within the cluster namespace with the cluster name and control plane label.
		err := c.List(ctx, machineList, client.InNamespace(csIsoNet.Namespace), client.MatchingLabels{
			clusterv1.ClusterNameLabel:         clusterName,
			clusterv1.MachineControlPlaneLabel: "",
		})
		if err != nil {
			log.Error(err, "Failed to get owned control plane Machines, skipping mapping.")

			return nil
		}

		log.V(4).Info("Looked up members with control plane label", "found", len(machineList.Items))

		results := make([]ctrl.Request, 0, len(machineList.Items))
		for _, machine := range machineList.Items {
			m := machine
			log.WithValues("machine", klog.KObj(&m))
			if m.Spec.InfrastructureRef.GroupVersionKind().Kind != "CloudStackMachine" {
				log.V(4).Info("Machine has an InfrastructureRef for a different type, will not add to reconciliation request.")

				continue
			}
			if m.Spec.InfrastructureRef.Name == "" {
				log.V(4).Info("Machine has an InfrastructureRef with an empty name, will not add to reconciliation request.")

				continue
			}
			log.WithValues("cloudStackMachine", klog.KRef(m.Spec.InfrastructureRef.Namespace, m.Spec.InfrastructureRef.Name))
			log.V(4).Info("Adding CloudStackMachine to reconciliation request.")
			results = append(results, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: m.Namespace, Name: m.Spec.InfrastructureRef.Name}})
		}

		return results
	}
}

// DebugPredicate returns a predicate that logs the event that triggered the reconciliation.
func DebugPredicate(logger logr.Logger) predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			log := logger.WithValues("predicate", "DebugPredicate", "eventType", "update")

			obj := fmt.Sprintf("%s/%s", e.ObjectOld.GetNamespace(), e.ObjectNew.GetName())
			diff, err := client.MergeFrom(e.ObjectOld).Data(e.ObjectNew)
			if err != nil {
				log.V(4).Error(err, "error generating diff")
			}
			log.V(4).Info("Update diff", "diff", string(diff), "obj", obj, "kind", e.ObjectOld.GetObjectKind().GroupVersionKind().Kind)

			return true
		},
		CreateFunc: func(e event.CreateEvent) bool {
			log := logger.WithValues("predicate", "DebugPredicate", "eventType", "create")

			obj := fmt.Sprintf("%s/%s", e.Object.GetNamespace(), e.Object.GetName())
			log.V(4).Info("Create", "obj", obj, "kind", e.Object.GetObjectKind().GroupVersionKind().Kind)
			if e.Object.GetObjectKind().GroupVersionKind().Kind == "" {
				log.V(4).Info("Kind is empty. Here's the whole object", "object", e.Object)
			}

			return true
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			log := logger.WithValues("predicate", "DebugPredicate", "eventType", "delete")

			obj := fmt.Sprintf("%s/%s", e.Object.GetNamespace(), e.Object.GetName())
			log.V(4).Info("Delete", "obj", obj, "kind", e.Object.GetObjectKind().GroupVersionKind().Kind)

			return true
		},
		GenericFunc: func(e event.GenericEvent) bool {
			log := logger.WithValues("predicate", "DebugPredicate", "eventType", "generic")

			obj := fmt.Sprintf("%s/%s", e.Object.GetNamespace(), e.Object.GetName())
			log.V(4).Info("Delete", "obj", obj, "kind", e.Object.GetObjectKind().GroupVersionKind().Kind)

			return true
		},
	}
}
