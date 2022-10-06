/*
Copyright 2022.

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
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrastructurev1alpha1 "github.com/capi-samples/cluster-api-provider-docker/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// DockerMachineReconciler reconciles a DockerMachine object
type DockerMachineReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=dockermachines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=dockermachines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=dockermachines/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the DockerMachine object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.12.2/pkg/reconcile
func (r *DockerMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconcile request received")

	// Fetch the DockerMachine instance
	dockerMachine := &infrastructurev1alpha1.DockerMachine{}
	err := r.Client.Get(ctx, req.NamespacedName, dockerMachine)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Fetch the Machine
	machine, err := util.GetOwnerMachine(ctx, r.Client, dockerMachine.ObjectMeta)
	if err != nil {
		logger.Error(err, "failed to get Owner Machine")
		return ctrl.Result{}, err
	}

	if machine == nil {
		logger.Info("Waiting for Machine Controller to set OwnerRef on DockerMachine")
		return ctrl.Result{}, nil
	}

	// Fetch the Cluster
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, dockerMachine.ObjectMeta)
	if err != nil {
		logger.Error(err, "DockerMachine owner Machine is missing cluster label or cluster does not exist")
		return ctrl.Result{}, err
	}

	if cluster == nil {
		logger.Info(fmt.Sprintf("Please associate this machine with a cluster using the label %s: <name of cluster>", clusterv1.ClusterLabelName))
		return ctrl.Result{}, nil
	}
	logger = logger.WithValues("cluster", cluster.Name)

	dockerCluster := &infrastructurev1alpha1.DockerCluster{}
	dockerClusterName := client.ObjectKey{
		Namespace: dockerMachine.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}

	if err = r.Client.Get(ctx, dockerClusterName, dockerCluster); err != nil {
		logger.Error(err, "failed to get docker cluster")
		return ctrl.Result{}, nil
	}

	helper, _ := patch.NewHelper(dockerMachine, r.Client)
	defer func() {
		if err = helper.Patch(ctx, dockerMachine); err != nil && reterr == nil {
			logger.Error(err, "failed to patch dockerMachine")
			reterr = err
		}
	}()

	// Return early if the object or Cluster is paused
	if annotations.IsPaused(cluster, dockerMachine) {
		logger.Info("dockerMachine or linked Cluster is marked as paused. Won't reconcile")
		return ctrl.Result{}, nil
	}

	// Add finalizer first if not exist to avoid the race condition between init and delete
	if !controllerutil.ContainsFinalizer(dockerMachine, infrastructurev1alpha1.MachineFinalizer) {
		controllerutil.AddFinalizer(dockerMachine, infrastructurev1alpha1.MachineFinalizer)
		return ctrl.Result{}, nil
	}

	// Handle deleted machines
	if !dockerMachine.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, cluster, machine, dockerMachine)
	}

	// Handle non-deleted machines
	return r.reconcileNormal(ctx, cluster, machine, dockerMachine)

}

func (r *DockerMachineReconciler) reconcileNormal(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, dockerMachine *infrastructurev1alpha1.DockerMachine) (_ ctrl.Result, retErr error) {
	logger := log.FromContext(ctx)

	// Check if the infrastructure is ready, otherwise return and wait for the cluster object to be updated
	if !cluster.Status.InfrastructureReady {
		logger.Info("Waiting for DockerCluster Controller to create cluster infrastructure")
		conditions.MarkFalse(dockerMachine, infrastructurev1alpha1.ContainerProvisionedCondition, infrastructurev1alpha1.WaitingForClusterInfrastructureReason, clusterv1.ConditionSeverityInfo, "")
		return ctrl.Result{}, nil
	}

	// if the machine is already provisioned, return
	if dockerMachine.Spec.ProviderID != nil {
		// ensure ready state is set.
		// This is required after move, because status is not moved to the target cluster.
		dockerMachine.Status.Ready = true
		return ctrl.Result{}, nil
	}

	// Make sure bootstrap data is available and populated.
	if machine.Spec.Bootstrap.DataSecretName == nil {
		if !util.IsControlPlaneMachine(machine) && !conditions.IsTrue(cluster, clusterv1.ControlPlaneInitializedCondition) {
			logger.Info("Waiting for the control plane to be initialized")
			conditions.MarkFalse(dockerMachine, infrastructurev1alpha1.ContainerProvisionedCondition, clusterv1.WaitingForControlPlaneAvailableReason, clusterv1.ConditionSeverityInfo, "")
			return ctrl.Result{}, nil
		}

		logger.Info("Waiting for the Bootstrap provider controller to set bootstrap data")
		conditions.MarkFalse(dockerMachine, infrastructurev1alpha1.ContainerProvisionedCondition, infrastructurev1alpha1.WaitingForBootstrapDataReason, clusterv1.ConditionSeverityInfo, "")
		return ctrl.Result{}, nil
	}

	// TODO: Add logic to create containers and patch dockerMachine
	return ctrl.Result{}, nil
}

func (r *DockerMachineReconciler) reconcileDelete(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, dockerMachine *infrastructurev1alpha1.DockerMachine) (_ ctrl.Result, retErr error) {
	patchHelper, err := patch.NewHelper(dockerMachine, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	conditions.MarkFalse(dockerMachine, infrastructurev1alpha1.ContainerProvisionedCondition, clusterv1.DeletingReason, clusterv1.ConditionSeverityInfo, "")
	return ctrl.Result{}, patchHelper.Patch(ctx, dockerMachine)

	// TODO: Add logic to delete machines
	// Machine is deleted so remove the finalizer.
	controllerutil.RemoveFinalizer(dockerMachine, infrastructurev1alpha1.MachineFinalizer)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DockerMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1alpha1.DockerMachine{}).
		Complete(r)
}
