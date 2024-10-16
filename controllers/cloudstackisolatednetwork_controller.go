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

package controllers

import (
	"context"
	"reflect"
	"strings"

	"github.com/pkg/errors"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	infrav1 "sigs.k8s.io/cluster-api-provider-cloudstack/api/v1beta3"
	csCtrlrUtils "sigs.k8s.io/cluster-api-provider-cloudstack/controllers/utils"
	"sigs.k8s.io/cluster-api-provider-cloudstack/pkg/cloud"
)

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=cloudstackisolatednetworks,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=cloudstackisolatednetworks/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=cloudstackisolatednetworks/finalizers,verbs=update

// CloudStackIsoNetReconciler reconciles a CloudStackZone object.
type CloudStackIsoNetReconciler struct {
	csCtrlrUtils.ReconcilerBase
}

// CloudStackIsoNetReconciliationRunner is a ReconciliationRunner with extensions specific to CloudStack isolated network reconciliation.
type CloudStackIsoNetReconciliationRunner struct {
	*csCtrlrUtils.ReconciliationRunner
	FailureDomain         *infrav1.CloudStackFailureDomain
	ReconciliationSubject *infrav1.CloudStackIsolatedNetwork
}

// Initialize a new CloudStackIsoNet reconciliation runner with concrete types and initialized member fields.
func NewCSIsoNetReconciliationRunner() *CloudStackIsoNetReconciliationRunner {
	// Set concrete type and init pointers.
	r := &CloudStackIsoNetReconciliationRunner{ReconciliationSubject: &infrav1.CloudStackIsolatedNetwork{}}
	r.FailureDomain = &infrav1.CloudStackFailureDomain{}
	// Set up the base runner. Initializes pointers and links reconciliation methods.
	r.ReconciliationRunner = csCtrlrUtils.NewRunner(r, r.ReconciliationSubject, "CloudStackIsolatedNetwork")

	return r
}

func (reconciler *CloudStackIsoNetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r := NewCSIsoNetReconciliationRunner()
	r.UsingBaseReconciler(reconciler.ReconcilerBase).ForRequest(req).WithRequestCtx(ctx)
	r.WithAdditionalCommonStages(
		r.GetFailureDomainByName(func() string { return r.ReconciliationSubject.Spec.FailureDomainName }, r.FailureDomain),
		r.AsFailureDomainUser(&r.FailureDomain.Spec),
	)

	return r.RunBaseReconciliationStages()
}

func (r *CloudStackIsoNetReconciliationRunner) Reconcile() (ctrl.Result, error) {
	controllerutil.AddFinalizer(r.ReconciliationSubject, infrav1.IsolatedNetworkFinalizer)

	// Setup isolated network, endpoint, egress, and load balancing.
	// Set endpoint of CloudStackCluster if it is not currently set. (uses patcher to do so)
	csClusterPatcher, err := patch.NewHelper(r.CSCluster, r.K8sClient)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "setting up CloudStackCluster patcher")
	}
	if r.FailureDomain.Spec.Zone.ID == "" {
		return r.RequeueWithMessage("Zone ID not resolved yet.")
	}

	if err := r.CSUser.GetOrCreateIsolatedNetwork(r.FailureDomain, r.ReconciliationSubject); err != nil {
		return ctrl.Result{}, err
	}
	// Tag the created network.
	if err := r.CSUser.AddClusterTag(cloud.ResourceTypeNetwork, r.ReconciliationSubject.Spec.ID, r.CSCluster); err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "tagging network with id %s", r.ReconciliationSubject.Spec.ID)
	}

	// Assign IP and configure API server load balancer, if enabled and this cluster is not externally managed.
	if !annotations.IsExternallyManaged(r.CSCluster) {
		pubIP, err := r.CSUser.AssociatePublicIPAddress(r.FailureDomain, r.ReconciliationSubject, r.CSCluster.Spec.ControlPlaneEndpoint.Host)
		if err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to associate public IP address")
		}
		r.ReconciliationSubject.Spec.ControlPlaneEndpoint.Host = pubIP.Ipaddress
		r.CSCluster.Spec.ControlPlaneEndpoint.Host = pubIP.Ipaddress
		r.ReconciliationSubject.Status.PublicIPID = pubIP.Id
		r.ReconciliationSubject.Status.PublicIPAddress = pubIP.Ipaddress

		if r.ReconciliationSubject.Status.APIServerLoadBalancer == nil {
			r.ReconciliationSubject.Status.APIServerLoadBalancer = &infrav1.LoadBalancer{}
		}
		r.ReconciliationSubject.Status.APIServerLoadBalancer.IPAddressID = pubIP.Id
		r.ReconciliationSubject.Status.APIServerLoadBalancer.IPAddress = pubIP.Ipaddress
		if err := r.CSUser.AddClusterTag(cloud.ResourceTypeIPAddress, pubIP.Id, r.CSCluster); err != nil {
			return ctrl.Result{}, errors.Wrapf(err,
				"adding cluster tag to public IP address with ID %s", pubIP.Id)
		}

		if err := r.CSUser.ReconcileLoadBalancer(r.FailureDomain, r.ReconciliationSubject, r.CSCluster); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "reconciling load balancer")
		}
	}

	if err := csClusterPatcher.Patch(r.RequestCtx, r.CSCluster); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "patching endpoint update to CloudStackCluster")
	}

	r.ReconciliationSubject.Status.Ready = true

	return ctrl.Result{}, nil
}

func (r *CloudStackIsoNetReconciliationRunner) ReconcileDelete() (ctrl.Result, error) {
	r.Log.Info("Deleting IsolatedNetwork.")
	if err := r.CSUser.DisposeIsoNetResources(r.ReconciliationSubject, r.CSCluster); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "no match found") {
			return ctrl.Result{}, err
		}
	}
	controllerutil.RemoveFinalizer(r.ReconciliationSubject, infrav1.IsolatedNetworkFinalizer)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (reconciler *CloudStackIsoNetReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	CloudStackClusterToCloudStackIsolatedNetworks, err := csCtrlrUtils.CloudStackClusterToCloudStackIsolatedNetworks(reconciler.K8sClient, &infrav1.CloudStackIsolatedNetworkList{}, reconciler.Scheme, ctrl.LoggerFrom(ctx))
	if err != nil {
		return errors.Wrap(err, "failed to create CloudStackClusterToCloudStackIsolatedNetworks mapper")
	}

	err = ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.CloudStackIsolatedNetwork{}).
		Watches(
			&infrav1.CloudStackCluster{},
			handler.EnqueueRequestsFromMapFunc(CloudStackClusterToCloudStackIsolatedNetworks),
			builder.WithPredicates(
				predicate.GenerationChangedPredicate{},
				predicate.Funcs{
					UpdateFunc: func(e event.UpdateEvent) bool {
						oldCSCluster := e.ObjectOld.(*infrav1.CloudStackCluster)
						newCSCluster := e.ObjectNew.(*infrav1.CloudStackCluster)

						// APIServerLoadBalancer disabled in both new and old
						if oldCSCluster.Spec.APIServerLoadBalancer == nil && newCSCluster.Spec.APIServerLoadBalancer == nil {
							return false
						}
						// APIServerLoadBalancer toggled
						if oldCSCluster.Spec.APIServerLoadBalancer == nil || newCSCluster.Spec.APIServerLoadBalancer == nil {
							return true
						}

						return !reflect.DeepEqual(oldCSCluster.Spec.APIServerLoadBalancer, newCSCluster.Spec.APIServerLoadBalancer)
					},
				},
			),
		).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(ctrl.LoggerFrom(ctx), reconciler.WatchFilterValue)).
		Complete(reconciler)
	if err != nil {
		return errors.Wrap(err, "failed setting up with a controller manager")
	}

	return nil
}
