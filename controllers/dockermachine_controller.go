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
	"encoding/base64"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/controllers/remote"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrastructurev1alpha1 "github.com/capi-samples/cluster-api-provider-docker/api/v1alpha1"
	"github.com/capi-samples/cluster-api-provider-docker/pkg/docker"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1beta1"
	"sigs.k8s.io/kind/pkg/cluster/constants"
)

// DockerMachineReconciler reconciles a DockerMachine object
type DockerMachineReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Tracker *remote.ClusterCacheTracker
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=dockermachines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=dockermachines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=dockermachines/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;machines/status,verbs=get;list;watch

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

	externalMachine, err := docker.NewMachine(ctx, cluster, machine.Name, nil)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to create helper for managing the externalMachine")
	}

	externalLoadBalancer, err := docker.NewLoadBalancer(ctx, cluster, dockerCluster)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to create helper for managing the externalLoadBalancer")
	}

	// Handle deleted machines
	if !dockerMachine.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, cluster, machine, dockerMachine, externalMachine, externalLoadBalancer)
	}

	// Handle non-deleted machines
	return r.reconcileNormal(ctx, cluster, machine, dockerMachine, externalMachine, externalLoadBalancer)

}

func (r *DockerMachineReconciler) reconcileNormal(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, dockerMachine *infrastructurev1alpha1.DockerMachine, externalMachine *docker.Machine, externalLoadBalancer *docker.LoadBalancer) (_ ctrl.Result, retErr error) {
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

	// Create the docker container hosting the machine
	role := constants.WorkerNodeRoleValue
	if util.IsControlPlaneMachine(machine) {
		role = constants.ControlPlaneNodeRoleValue
	}

	// Create the machine if not existing yet
	if !externalMachine.Exists() {
		if err := externalMachine.Create(ctx, dockerMachine.Spec.CustomImage, role, machine.Spec.Version, docker.FailureDomainLabel(machine.Spec.FailureDomain), nil); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to create worker DockerMachine")
		}
	}

	// if the machine is a control plane update the load balancer configuration
	// we should only do this once, as reconfiguration more or less ensures
	// node ref setting fails
	if util.IsControlPlaneMachine(machine) && !dockerMachine.Status.LoadBalancerConfigured {
		if err := externalLoadBalancer.UpdateConfiguration(ctx); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to update DockerCluster.loadbalancer configuration")
		}
		dockerMachine.Status.LoadBalancerConfigured = true
	}

	patchHelper, err := patch.NewHelper(dockerMachine, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Update the ContainerProvisionedCondition condition
	conditions.MarkTrue(dockerMachine, infrastructurev1alpha1.ContainerProvisionedCondition)

	// At, this stage, we are ready for bootstrap. However, if the BootstrapExecSucceededCondition is missing we add it and we
	// issue an patch so the user can see the change of state before the bootstrap actually starts.
	// NOTE: usually controller should not rely on status they are setting, but on the observed state; however
	// in this case we are doing this because we explicitly want to give a feedback to users.
	if !conditions.Has(dockerMachine, infrastructurev1alpha1.BootstrapExecSucceededCondition) {
		conditions.MarkFalse(dockerMachine, infrastructurev1alpha1.BootstrapExecSucceededCondition, infrastructurev1alpha1.BootstrappingReason, clusterv1.ConditionSeverityInfo, "")
		if err = patchHelper.Patch(ctx, dockerMachine); err != nil && retErr == nil {
			logger.Error(err, "failed to patch dockerMachine")
			retErr = err
		}
	}

	// if the machine isn't bootstrapped, only then run bootstrap scripts
	if !dockerMachine.Spec.Bootstrapped {
		timeoutCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		if err := externalMachine.CheckForBootstrapSuccess(timeoutCtx, false); err != nil {
			bootstrapData, format, err := r.getBootstrapData(timeoutCtx, machine)
			if err != nil {
				logger.Error(err, "failed to get bootstrap data")
				return ctrl.Result{}, err
			}

			// Run the bootstrap script. Simulates cloud-init/Ignition.
			if err := externalMachine.ExecBootstrap(timeoutCtx, bootstrapData, format); err != nil {
				conditions.MarkFalse(dockerMachine, infrastructurev1alpha1.BootstrapExecSucceededCondition, infrastructurev1alpha1.BootstrapFailedReason, clusterv1.ConditionSeverityWarning, "Repeating bootstrap")
				return ctrl.Result{}, errors.Wrap(err, "failed to exec DockerMachine bootstrap")
			}
			// Check for bootstrap success
			if err := externalMachine.CheckForBootstrapSuccess(timeoutCtx, true); err != nil {
				conditions.MarkFalse(dockerMachine, infrastructurev1alpha1.BootstrapExecSucceededCondition, infrastructurev1alpha1.BootstrapFailedReason, clusterv1.ConditionSeverityWarning, "Repeating bootstrap")
				return ctrl.Result{}, errors.Wrap(err, "failed to check for existence of bootstrap success file at /run/cluster-api/bootstrap-success.complete")
			}
		}
		dockerMachine.Spec.Bootstrapped = true
	}

	// Update the BootstrapExecSucceededCondition condition
	conditions.MarkTrue(dockerMachine, infrastructurev1alpha1.BootstrapExecSucceededCondition)

	if err := setMachineAddress(ctx, dockerMachine, externalMachine); err != nil {
		logger.Error(err, "failed to set the machine address")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Usually a cloud provider will do this, but there is no docker-cloud provider
	remoteClient, err := r.Tracker.GetClient(ctx, client.ObjectKeyFromObject(cluster))
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to generate workload cluster client")
	}
	if err := externalMachine.SetNodeProviderID(ctx, remoteClient); err != nil {
		if errors.As(err, &docker.ContainerNotRunningError{}) {
			return ctrl.Result{}, errors.Wrap(err, "failed to patch the Kubernetes node with the machine providerID")
		}
		logger.Error(err, "failed to patch the Kubernetes node with the machine providerID")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	// Set ProviderID so the Cluster API Machine Controller can pull it
	providerID := externalMachine.ProviderID()
	dockerMachine.Spec.ProviderID = &providerID
	dockerMachine.Status.Ready = true
	conditions.MarkTrue(dockerMachine, infrastructurev1alpha1.ContainerProvisionedCondition)

	return ctrl.Result{}, nil
}

func (r *DockerMachineReconciler) reconcileDelete(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, dockerMachine *infrastructurev1alpha1.DockerMachine, externalMachine *docker.Machine, externalLoadBalancer *docker.LoadBalancer) (_ ctrl.Result, retErr error) {
	logger := log.FromContext(ctx)

	// Set the ContainerProvisionedCondition reporting delete is started, and issue a patch in order to make
	// this visible to the users.
	patchHelper, err := patch.NewHelper(dockerMachine, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	conditions.MarkFalse(dockerMachine, infrastructurev1alpha1.ContainerProvisionedCondition, clusterv1.DeletingReason, clusterv1.ConditionSeverityInfo, "")

	if err = patchHelper.Patch(ctx, dockerMachine); err != nil && retErr == nil {
		logger.Error(err, "failed to patch dockerMachine")
		retErr = err
	}

	// delete the machine
	if err := externalMachine.Delete(ctx); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to delete DockerMachine")
	}

	// if the deleted machine is a control-plane node, remove it from the load balancer configuration;
	if util.IsControlPlaneMachine(machine) {
		if err := externalLoadBalancer.UpdateConfiguration(ctx); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to update DockerCluster.loadbalancer configuration")
		}
	}

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

// setMachineAddress gets the address from the container corresponding to a docker node and sets it on the Machine object.
func setMachineAddress(ctx context.Context, dockerMachine *infrastructurev1alpha1.DockerMachine, externalMachine *docker.Machine) error {
	machineAddress, err := externalMachine.Address(ctx)
	if err != nil {
		return err
	}

	dockerMachine.Status.Addresses = []clusterv1.MachineAddress{
		{
			Type:    clusterv1.MachineHostName,
			Address: externalMachine.ContainerName(),
		},
		{
			Type:    clusterv1.MachineInternalIP,
			Address: machineAddress,
		},
		{
			Type:    clusterv1.MachineExternalIP,
			Address: machineAddress,
		},
	}
	return nil
}

func (r *DockerMachineReconciler) getBootstrapData(ctx context.Context, machine *clusterv1.Machine) (string, bootstrapv1.Format, error) {
	if machine.Spec.Bootstrap.DataSecretName == nil {
		return "", "", errors.New("error retrieving bootstrap data: linked Machine's bootstrap.dataSecretName is nil")
	}

	s := &corev1.Secret{}
	key := client.ObjectKey{Namespace: machine.GetNamespace(), Name: *machine.Spec.Bootstrap.DataSecretName}
	if err := r.Client.Get(ctx, key, s); err != nil {
		return "", "", errors.Wrapf(err, "failed to retrieve bootstrap data secret for DockerMachine %s", klog.KObj(machine))
	}

	value, ok := s.Data["value"]
	if !ok {
		return "", "", errors.New("error retrieving bootstrap data: secret value key is missing")
	}

	format := s.Data["format"]
	if len(format) == 0 {
		format = []byte(bootstrapv1.CloudConfig)
	}

	return base64.StdEncoding.EncodeToString(value), bootstrapv1.Format(format), nil
}
