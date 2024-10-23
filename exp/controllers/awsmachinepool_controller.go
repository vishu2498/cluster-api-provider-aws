/*
Copyright 2020 The Kubernetes Authors.

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

// Package controllers provides experimental API controllers.
package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/controllers"
	ekscontrolplanev1 "sigs.k8s.io/cluster-api-provider-aws/v2/controlplane/eks/api/v1beta2"
	expinfrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/exp/api/v1beta2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/scope"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services"
	asg "sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/autoscaling"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/ec2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/logger"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	expclusterv1 "sigs.k8s.io/cluster-api/exp/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/predicates"
)

// AWSMachinePoolReconciler reconciles a AWSMachinePool object.
type AWSMachinePoolReconciler struct {
	client.Client
	Recorder                     record.EventRecorder
	WatchFilterValue             string
	asgServiceFactory            func(cloud.ClusterScoper) services.ASGInterface
	ec2ServiceFactory            func(scope.EC2Scope) services.EC2Interface
	reconcileServiceFactory      func(scope.EC2Scope) services.MachinePoolReconcileInterface
	TagUnmanagedNetworkResources bool
}

func (r *AWSMachinePoolReconciler) getASGService(scope cloud.ClusterScoper) services.ASGInterface {
	if r.asgServiceFactory != nil {
		return r.asgServiceFactory(scope)
	}
	return asg.NewService(scope)
}

func (r *AWSMachinePoolReconciler) getEC2Service(scope scope.EC2Scope) services.EC2Interface {
	if r.ec2ServiceFactory != nil {
		return r.ec2ServiceFactory(scope)
	}

	return ec2.NewService(scope)
}

func (r *AWSMachinePoolReconciler) getReconcileService(scope scope.EC2Scope) services.MachinePoolReconcileInterface {
	if r.reconcileServiceFactory != nil {
		return r.reconcileServiceFactory(scope)
	}

	return ec2.NewService(scope)
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmachinepools,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmachinepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinepools;machinepools/status,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=secrets;,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile is the reconciliation loop for AWSMachinePool.
func (r *AWSMachinePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	log := logger.FromContext(ctx)

	// Fetch the AWSMachinePool .
	awsMachinePool := &expinfrav1.AWSMachinePool{}
	err := r.Get(ctx, req.NamespacedName, awsMachinePool)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Fetch the CAPI MachinePool
	machinePool, err := getOwnerMachinePool(ctx, r.Client, awsMachinePool.ObjectMeta)
	if err != nil {
		return reconcile.Result{}, err
	}
	if machinePool == nil {
		log.Info("MachinePool Controller has not yet set OwnerRef")
		return reconcile.Result{}, nil
	}
	log = log.WithValues("machinePool", klog.KObj(machinePool))

	// Fetch the Cluster.
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machinePool.ObjectMeta)
	if err != nil {
		log.Info("MachinePool is missing cluster label or cluster does not exist")
		return reconcile.Result{}, nil
	}

	log = log.WithValues("cluster", klog.KObj(cluster))

	infraCluster, err := r.getInfraCluster(ctx, log, cluster, awsMachinePool)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting infra provider cluster or control plane object: %w", err)
	}
	if infraCluster == nil {
		log.Info("AWSCluster or AWSManagedControlPlane is not ready yet")
		return ctrl.Result{}, nil
	}

	// Create the machine pool scope
	machinePoolScope, err := scope.NewMachinePoolScope(scope.MachinePoolScopeParams{
		Client:         r.Client,
		Logger:         log,
		Cluster:        cluster,
		MachinePool:    machinePool,
		InfraCluster:   infraCluster,
		AWSMachinePool: awsMachinePool,
	})
	if err != nil {
		log.Error(err, "failed to create scope")
		return ctrl.Result{}, err
	}

	// Always close the scope when exiting this function so we can persist any AWSMachine changes.
	defer func() {
		// set Ready condition before AWSMachinePool is patched
		conditions.SetSummary(machinePoolScope.AWSMachinePool,
			conditions.WithConditions(
				expinfrav1.ASGReadyCondition,
				expinfrav1.LaunchTemplateReadyCondition,
			),
			conditions.WithStepCounterIfOnly(
				expinfrav1.ASGReadyCondition,
				expinfrav1.LaunchTemplateReadyCondition,
			),
		)

		if err := machinePoolScope.Close(); err != nil && reterr == nil {
			reterr = err
		}
	}()

	// Patch now so that the status and selectors are available.
	awsMachinePool.Status.InfrastructureMachineKind = "AWSMachine"
	if err := machinePoolScope.PatchObject(); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to patch AWSMachinePool status")
	}

	switch infraScope := infraCluster.(type) {
	case *scope.ManagedControlPlaneScope:
		if !awsMachinePool.ObjectMeta.DeletionTimestamp.IsZero() {
			return ctrl.Result{}, r.reconcileDelete(ctx, machinePoolScope, infraScope, infraScope)
		}

		return r.reconcileNormal(ctx, machinePoolScope, infraScope, infraScope)
	case *scope.ClusterScope:
		if !awsMachinePool.ObjectMeta.DeletionTimestamp.IsZero() {
			return ctrl.Result{}, r.reconcileDelete(ctx, machinePoolScope, infraScope, infraScope)
		}

		return r.reconcileNormal(ctx, machinePoolScope, infraScope, infraScope)
	default:
		return ctrl.Result{}, errors.New("infraCluster has unknown type")
	}
}

func (r *AWSMachinePoolReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(options).
		For(&expinfrav1.AWSMachinePool{}).
		Watches(
			&expclusterv1.MachinePool{},
			handler.EnqueueRequestsFromMapFunc(machinePoolToInfrastructureMapFunc(expinfrav1.GroupVersion.WithKind("AWSMachinePool"))),
		).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(logger.FromContext(ctx).GetLogger(), r.WatchFilterValue)).
		Complete(r)
}

func (r *AWSMachinePoolReconciler) reconcileNormal(ctx context.Context, machinePoolScope *scope.MachinePoolScope, clusterScope cloud.ClusterScoper, ec2Scope scope.EC2Scope) (ctrl.Result, error) {
	clusterScope.Info("Reconciling AWSMachinePool")

	// If the AWSMachine is in an error state, return early.
	if machinePoolScope.HasFailed() {
		machinePoolScope.Info("Error state detected, skipping reconciliation")

		// TODO: If we are in a failed state, delete the secret regardless of instance state

		return ctrl.Result{}, nil
	}

	// If the AWSMachinepool doesn't have our finalizer, add it
	if controllerutil.AddFinalizer(machinePoolScope.AWSMachinePool, expinfrav1.MachinePoolFinalizer) {
		// Register finalizer immediately to avoid orphaning AWS resources
		if err := machinePoolScope.PatchObject(); err != nil {
			return ctrl.Result{}, err
		}
	}

	if !machinePoolScope.Cluster.Status.InfrastructureReady {
		machinePoolScope.Info("Cluster infrastructure is not ready yet")
		conditions.MarkFalse(machinePoolScope.AWSMachinePool, expinfrav1.ASGReadyCondition, infrav1.WaitingForClusterInfrastructureReason, clusterv1.ConditionSeverityInfo, "")
		return ctrl.Result{}, nil
	}

	// Make sure bootstrap data is available and populated
	if machinePoolScope.MachinePool.Spec.Template.Spec.Bootstrap.DataSecretName == nil {
		machinePoolScope.Info("Bootstrap data secret reference is not yet available")
		conditions.MarkFalse(machinePoolScope.AWSMachinePool, expinfrav1.ASGReadyCondition, infrav1.WaitingForBootstrapDataReason, clusterv1.ConditionSeverityInfo, "")
		return ctrl.Result{}, nil
	}

	ec2Svc := r.getEC2Service(ec2Scope)
	asgsvc := r.getASGService(clusterScope)
	reconSvc := r.getReconcileService(ec2Scope)

	// Find existing ASG
	asg, err := r.findASG(machinePoolScope, asgsvc)
	if err != nil {
		conditions.MarkUnknown(machinePoolScope.AWSMachinePool, expinfrav1.ASGReadyCondition, expinfrav1.ASGNotFoundReason, err.Error())
		return ctrl.Result{}, err
	}

	canUpdateLaunchTemplate := func() (bool, error) {
		// If there is a change: before changing the template, check if there exist an ongoing instance refresh,
		// because only 1 instance refresh can be "InProgress". If template is updated when refresh cannot be started,
		// that change will not trigger a refresh. Do not start an instance refresh if only userdata changed.
		if asg == nil {
			// If the ASG hasn't been created yet, there is no need to check if we can start the instance refresh.
			// But we want to update the LaunchTemplate because an error in the LaunchTemplate may be blocking the ASG creation.
			return true, nil
		}
		return asgsvc.CanStartASGInstanceRefresh(machinePoolScope)
	}
	runPostLaunchTemplateUpdateOperation := func() error {
		// skip instance refresh if ASG is not created yet
		if asg == nil {
			machinePoolScope.Debug("ASG does not exist yet, skipping instance refresh")
			return nil
		}
		// skip instance refresh if explicitly disabled
		if machinePoolScope.AWSMachinePool.Spec.RefreshPreferences != nil && machinePoolScope.AWSMachinePool.Spec.RefreshPreferences.Disable {
			machinePoolScope.Debug("instance refresh disabled, skipping instance refresh")
			return nil
		}
		// After creating a new version of launch template, instance refresh is required
		// to trigger a rolling replacement of all previously launched instances.
		// If ONLY the userdata changed, previously launched instances continue to use the old launch
		// template.
		//
		// FIXME(dlipovetsky,sedefsavas): If the controller terminates, or the StartASGInstanceRefresh returns an error,
		// this conditional will not evaluate to true the next reconcile. If any machines use an older
		// Launch Template version, and the difference between the older and current versions is _more_
		// than userdata, we should start an Instance Refresh.
		machinePoolScope.Info("starting instance refresh", "number of instances", machinePoolScope.MachinePool.Spec.Replicas)
		return asgsvc.StartASGInstanceRefresh(machinePoolScope)
	}
	if err := reconSvc.ReconcileLaunchTemplate(machinePoolScope, ec2Svc, canUpdateLaunchTemplate, runPostLaunchTemplateUpdateOperation); err != nil {
		r.Recorder.Eventf(machinePoolScope.AWSMachinePool, corev1.EventTypeWarning, "FailedLaunchTemplateReconcile", "Failed to reconcile launch template: %v", err)
		machinePoolScope.Error(err, "failed to reconcile launch template")
		return ctrl.Result{}, err
	}

	// set the LaunchTemplateReady condition
	conditions.MarkTrue(machinePoolScope.AWSMachinePool, expinfrav1.LaunchTemplateReadyCondition)

	if asg == nil {
		// Create new ASG
		if err := r.createPool(machinePoolScope, clusterScope); err != nil {
			conditions.MarkFalse(machinePoolScope.AWSMachinePool, expinfrav1.ASGReadyCondition, expinfrav1.ASGProvisionFailedReason, clusterv1.ConditionSeverityError, err.Error())
			return ctrl.Result{}, err
		}
		return ctrl.Result{
			RequeueAfter: 15 * time.Second,
		}, nil
	}

	awsMachineList, err := getAWSMachines(ctx, machinePoolScope.MachinePool, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := createAWSMachinesIfNotExists(ctx, awsMachineList, machinePoolScope.MachinePool, &machinePoolScope.AWSMachinePool.ObjectMeta, &machinePoolScope.AWSMachinePool.TypeMeta, asg, machinePoolScope.GetLogger(), r.Client, ec2Svc); err != nil {
		machinePoolScope.SetNotReady()
		conditions.MarkFalse(machinePoolScope.AWSMachinePool, clusterv1.ReadyCondition, expinfrav1.AWSMachineCreationFailed, clusterv1.ConditionSeverityWarning, "%s", err.Error())
		return ctrl.Result{}, fmt.Errorf("failed to create awsmachines: %w", err)
	}

	if err := deleteOrphanedAWSMachines(ctx, awsMachineList, asg, machinePoolScope.GetLogger(), r.Client); err != nil {
		machinePoolScope.SetNotReady()
		conditions.MarkFalse(machinePoolScope.AWSMachinePool, clusterv1.ReadyCondition, expinfrav1.AWSMachineDeletionFailed, clusterv1.ConditionSeverityWarning, "%s", err.Error())
		return ctrl.Result{}, fmt.Errorf("failed to clean up awsmachines: %w", err)
	}

	if annotations.ReplicasManagedByExternalAutoscaler(machinePoolScope.MachinePool) {
		// Set MachinePool replicas to the ASG DesiredCapacity
		if *machinePoolScope.MachinePool.Spec.Replicas != *asg.DesiredCapacity {
			machinePoolScope.Info("Setting MachinePool replicas to ASG DesiredCapacity",
				"local", machinePoolScope.MachinePool.Spec.Replicas,
				"external", asg.DesiredCapacity)
			machinePoolScope.MachinePool.Spec.Replicas = asg.DesiredCapacity
			if err := machinePoolScope.PatchCAPIMachinePoolObject(ctx); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	if err := r.updatePool(machinePoolScope, clusterScope, asg); err != nil {
		machinePoolScope.Error(err, "error updating AWSMachinePool")
		return ctrl.Result{}, err
	}

	launchTemplateID := machinePoolScope.GetLaunchTemplateIDStatus()
	asgName := machinePoolScope.Name()
	resourceServiceToUpdate := []scope.ResourceServiceToUpdate{
		{
			ResourceID:      &launchTemplateID,
			ResourceService: ec2Svc,
		},
		{
			ResourceID:      &asgName,
			ResourceService: asgsvc,
		},
	}
	err = reconSvc.ReconcileTags(machinePoolScope, resourceServiceToUpdate)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "error updating tags")
	}

	// Make sure Spec.ProviderID is always set.
	machinePoolScope.AWSMachinePool.Spec.ProviderID = asg.ID
	providerIDList := make([]string, len(asg.Instances))

	for i, ec2 := range asg.Instances {
		providerIDList[i] = fmt.Sprintf("aws:///%s/%s", ec2.AvailabilityZone, ec2.ID)
	}

	machinePoolScope.SetAnnotation("cluster-api-provider-aws", "true")

	machinePoolScope.AWSMachinePool.Spec.ProviderIDList = providerIDList
	machinePoolScope.AWSMachinePool.Status.Replicas = int32(len(providerIDList))
	machinePoolScope.AWSMachinePool.Status.Ready = true
	conditions.MarkTrue(machinePoolScope.AWSMachinePool, expinfrav1.ASGReadyCondition)

	err = machinePoolScope.UpdateInstanceStatuses(ctx, asg.Instances)
	if err != nil {
		machinePoolScope.Error(err, "failed updating instances", "instances", asg.Instances)
	}

	return ctrl.Result{
		// Regularly update `AWSMachine` objects, for example if ASG was scaled or refreshed instances
		// TODO: Requeueing interval can be removed or prolonged once reconciliation of ASG EC2 instances
		//       can be triggered by events.
		RequeueAfter: 3 * time.Minute,
	}, nil
}

func (r *AWSMachinePoolReconciler) reconcileDelete(ctx context.Context, machinePoolScope *scope.MachinePoolScope, clusterScope cloud.ClusterScoper, ec2Scope scope.EC2Scope) error {
	clusterScope.Info("Handling deleted AWSMachinePool")
	if err := reconcileDeleteAWSMachines(ctx, machinePoolScope.MachinePool, r.Client, machinePoolScope.GetLogger()); err != nil {
		return err
	}

	ec2Svc := r.getEC2Service(ec2Scope)
	asgSvc := r.getASGService(clusterScope)

	asg, err := r.findASG(machinePoolScope, asgSvc)
	if err != nil {
		return err
	}

	if asg == nil {
		machinePoolScope.Warn("Unable to locate ASG")
		r.Recorder.Eventf(machinePoolScope.AWSMachinePool, corev1.EventTypeNormal, expinfrav1.ASGNotFoundReason, "Unable to find matching ASG")
	} else {
		machinePoolScope.SetASGStatus(asg.Status)
		switch asg.Status {
		case expinfrav1.ASGStatusDeleteInProgress:
			// ASG is already deleting
			machinePoolScope.SetNotReady()
			conditions.MarkFalse(machinePoolScope.AWSMachinePool, expinfrav1.ASGReadyCondition, expinfrav1.ASGDeletionInProgress, clusterv1.ConditionSeverityWarning, "")
			r.Recorder.Eventf(machinePoolScope.AWSMachinePool, corev1.EventTypeWarning, "DeletionInProgress", "ASG deletion in progress: %q", asg.Name)
			machinePoolScope.Info("ASG is already deleting", "name", asg.Name)
		default:
			machinePoolScope.Info("Deleting ASG", "id", asg.Name, "status", asg.Status)
			if err := asgSvc.DeleteASGAndWait(asg.Name); err != nil {
				r.Recorder.Eventf(machinePoolScope.AWSMachinePool, corev1.EventTypeWarning, "FailedDelete", "Failed to delete ASG %q: %v", asg.Name, err)
				return errors.Wrap(err, "failed to delete ASG")
			}
		}
	}

	launchTemplateID := machinePoolScope.AWSMachinePool.Status.LaunchTemplateID
	launchTemplate, _, _, err := ec2Svc.GetLaunchTemplate(machinePoolScope.LaunchTemplateName())
	if err != nil {
		return err
	}

	if launchTemplate == nil {
		machinePoolScope.Debug("Unable to locate launch template")
		r.Recorder.Eventf(machinePoolScope.AWSMachinePool, corev1.EventTypeNormal, expinfrav1.ASGNotFoundReason, "Unable to find matching ASG")
		controllerutil.RemoveFinalizer(machinePoolScope.AWSMachinePool, expinfrav1.MachinePoolFinalizer)
		return nil
	}

	machinePoolScope.Info("deleting launch template", "name", launchTemplate.Name)
	if err := ec2Svc.DeleteLaunchTemplate(launchTemplateID); err != nil {
		r.Recorder.Eventf(machinePoolScope.AWSMachinePool, corev1.EventTypeWarning, "FailedDelete", "Failed to delete launch template %q: %v", launchTemplate.Name, err)
		return errors.Wrap(err, "failed to delete ASG")
	}

	machinePoolScope.Info("successfully deleted AutoScalingGroup and Launch Template")

	// remove finalizer
	controllerutil.RemoveFinalizer(machinePoolScope.AWSMachinePool, expinfrav1.MachinePoolFinalizer)

	return nil
}

func reconcileDeleteAWSMachines(ctx context.Context, mp *expclusterv1.MachinePool, client client.Client, l logr.Logger) error {
	awsMachineList, err := getAWSMachines(ctx, mp, client)
	if err != nil {
		return err
	}
	for i := range awsMachineList.Items {
		awsMachine := awsMachineList.Items[i]
		if awsMachine.DeletionTimestamp.IsZero() {
			continue
		}
		logger := l.WithValues("awsmachine", klog.KObj(&awsMachine))
		// delete the owner Machine resource for the AWSMachine so that CAPI can clean up gracefully
		machine, err := util.GetOwnerMachine(ctx, client, awsMachine.ObjectMeta)
		if err != nil {
			logger.V(2).Info("Failed to get owner Machine", "err", err.Error())
			continue
		}

		if err := client.Delete(ctx, machine); err != nil {
			logger.V(2).Info("Failed to delete owner Machine", "err", err.Error())
		}
	}
	return nil
}

func getAWSMachines(ctx context.Context, mp *expclusterv1.MachinePool, kubeClient client.Client) (*infrav1.AWSMachineList, error) {
	awsMachineList := &infrav1.AWSMachineList{}
	labels := map[string]string{
		clusterv1.MachinePoolNameLabel: mp.Name,
		clusterv1.ClusterNameLabel:     mp.Spec.ClusterName,
	}
	if err := kubeClient.List(ctx, awsMachineList, client.InNamespace(mp.Namespace), client.MatchingLabels(labels)); err != nil {
		return nil, err
	}
	return awsMachineList, nil
}

func createAWSMachinesIfNotExists(ctx context.Context, awsMachineList *infrav1.AWSMachineList, mp *expclusterv1.MachinePool, infraMachinePoolMeta *metav1.ObjectMeta, infraMachinePoolType *metav1.TypeMeta, existingASG *expinfrav1.AutoScalingGroup, l logr.Logger, client client.Client, ec2Svc services.EC2Interface) error {
	l.V(4).Info("Creating missing AWSMachines")

	providerIDToAWSMachine := make(map[string]infrav1.AWSMachine, len(awsMachineList.Items))
	for i := range awsMachineList.Items {
		awsMachine := awsMachineList.Items[i]
		if awsMachine.Spec.ProviderID == nil || *awsMachine.Spec.ProviderID == "" {
			continue
		}
		providerID := *awsMachine.Spec.ProviderID
		providerIDToAWSMachine[providerID] = awsMachine
	}

	for i := range existingASG.Instances {
		instanceID := existingASG.Instances[i].ID
		providerID := fmt.Sprintf("aws:///%s/%s", existingASG.Instances[i].AvailabilityZone, instanceID)

		instanceLogger := l.WithValues("providerID", providerID, "instanceID", instanceID, "asg", existingASG.Name)
		instanceLogger.V(4).Info("Checking if machine pool AWSMachine is up to date")
		if _, exists := providerIDToAWSMachine[providerID]; exists {
			continue
		}

		instance, err := ec2Svc.InstanceIfExists(&instanceID)
		if errors.Is(err, ec2.ErrInstanceNotFoundByID) {
			instanceLogger.V(4).Info("Instance not found, it may have already been deleted")
			continue
		}
		if err != nil {
			return fmt.Errorf("failed to look up EC2 instance %q: %w", instanceID, err)
		}

		securityGroups := make([]infrav1.AWSResourceReference, 0, len(instance.SecurityGroupIDs))
		for j := range instance.SecurityGroupIDs {
			securityGroups = append(securityGroups, infrav1.AWSResourceReference{
				ID: aws.String(instance.SecurityGroupIDs[j]),
			})
		}

		awsMachine := &infrav1.AWSMachine{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    mp.Namespace,
				GenerateName: fmt.Sprintf("%s-", existingASG.Name),
				Labels: map[string]string{
					clusterv1.MachinePoolNameLabel: mp.Name,
					clusterv1.ClusterNameLabel:     mp.Spec.ClusterName,
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         infraMachinePoolType.APIVersion,
						Kind:               infraMachinePoolType.Kind,
						Name:               infraMachinePoolMeta.Name,
						BlockOwnerDeletion: ptr.To(true),
						UID:                infraMachinePoolMeta.UID,
					},
				},
			},
			Spec: infrav1.AWSMachineSpec{
				ProviderID: aws.String(providerID),
				InstanceID: aws.String(instanceID),

				// Store some extra fields for informational purposes (not needed by CAPA)
				AMI: infrav1.AMIReference{
					ID: aws.String(instance.ImageID),
				},
				InstanceType:             instance.Type,
				PublicIP:                 aws.Bool(instance.PublicIP != nil),
				SSHKeyName:               instance.SSHKeyName,
				InstanceMetadataOptions:  instance.InstanceMetadataOptions,
				IAMInstanceProfile:       instance.IAMProfile,
				AdditionalSecurityGroups: securityGroups,
				Subnet:                   &infrav1.AWSResourceReference{ID: aws.String(instance.SubnetID)},
				RootVolume:               instance.RootVolume,
				NonRootVolumes:           instance.NonRootVolumes,
				NetworkInterfaces:        instance.NetworkInterfaces,
				CloudInit:                infrav1.CloudInit{},
				SpotMarketOptions:        instance.SpotMarketOptions,
				Tenancy:                  instance.Tenancy,
			},
		}
		instanceLogger.V(4).Info("Creating AWSMachine")
		if err := client.Create(ctx, awsMachine); err != nil {
			return fmt.Errorf("failed to create AWSMachine: %w", err)
		}
	}
	return nil
}

func deleteOrphanedAWSMachines(ctx context.Context, awsMachineList *infrav1.AWSMachineList, existingASG *expinfrav1.AutoScalingGroup, l logr.Logger, client client.Client) error {
	l.V(4).Info("Deleting orphaned AWSMachines")
	providerIDToInstance := make(map[string]infrav1.Instance, len(existingASG.Instances))
	for i := range existingASG.Instances {
		providerID := fmt.Sprintf("aws:///%s/%s", existingASG.Instances[i].AvailabilityZone, existingASG.Instances[i].ID)
		providerIDToInstance[providerID] = existingASG.Instances[i]
	}

	for i := range awsMachineList.Items {
		awsMachine := awsMachineList.Items[i]
		if awsMachine.Spec.ProviderID == nil || *awsMachine.Spec.ProviderID == "" {
			continue
		}

		providerID := *awsMachine.Spec.ProviderID
		if _, exists := providerIDToInstance[providerID]; exists {
			continue
		}

		machine, err := util.GetOwnerMachine(ctx, client, awsMachine.ObjectMeta)
		if err != nil {
			return fmt.Errorf("failed to get owner Machine for %s/%s: %w", awsMachine.Namespace, awsMachine.Name, err)
		}
		machineLogger := l.WithValues("machine", klog.KObj(machine), "awsmachine", klog.KObj(&awsMachine), "ProviderID", providerID)
		machineLogger.V(4).Info("Deleting orphaned Machine")
		if machine == nil {
			machineLogger.Info("No machine owner found for AWSMachine, deleting AWSMachine anyway.")
			if err := client.Delete(ctx, &awsMachine); err != nil {
				return fmt.Errorf("failed to delete orphan AWSMachine %s/%s: %w", awsMachine.Namespace, awsMachine.Name, err)
			}
			machineLogger.V(4).Info("Deleted AWSMachine")
			continue
		}

		if err := client.Delete(ctx, machine); err != nil {
			return fmt.Errorf("failed to delete orphan Machine %s/%s: %w", machine.Namespace, machine.Name, err)
		}
		machineLogger.V(4).Info("Deleted Machine")
	}
	return nil
}

func (r *AWSMachinePoolReconciler) updatePool(machinePoolScope *scope.MachinePoolScope, clusterScope cloud.ClusterScoper, existingASG *expinfrav1.AutoScalingGroup) error {
	asgSvc := r.getASGService(clusterScope)

	subnetIDs, err := asgSvc.SubnetIDs(machinePoolScope)
	if err != nil {
		return errors.Wrapf(err, "fail to get subnets for ASG")
	}
	machinePoolScope.Debug("determining if subnets change in machinePoolScope",
		"subnets of machinePoolScope", subnetIDs,
		"subnets of existing asg", existingASG.Subnets)
	less := func(a, b string) bool { return a < b }
	subnetDiff := cmp.Diff(subnetIDs, existingASG.Subnets, cmpopts.SortSlices(less))
	if subnetDiff != "" {
		machinePoolScope.Debug("asg subnet diff detected", "diff", subnetDiff)
	}

	asgDiff := diffASG(machinePoolScope, existingASG)
	if asgDiff != "" {
		machinePoolScope.Debug("asg diff detected", "asgDiff", asgDiff, "subnetDiff", subnetDiff)
	}
	if asgDiff != "" || subnetDiff != "" {
		machinePoolScope.Info("updating AutoScalingGroup")

		if err := asgSvc.UpdateASG(machinePoolScope); err != nil {
			r.Recorder.Eventf(machinePoolScope.AWSMachinePool, corev1.EventTypeWarning, "FailedUpdate", "Failed to update ASG: %v", err)
			return errors.Wrap(err, "unable to update ASG")
		}
	}

	suspendedProcessesSlice := machinePoolScope.AWSMachinePool.Spec.SuspendProcesses.ConvertSetValuesToStringSlice()
	if !cmp.Equal(existingASG.CurrentlySuspendProcesses, suspendedProcessesSlice) {
		clusterScope.Info("reconciling processes", "suspend-processes", suspendedProcessesSlice)
		var (
			toBeSuspended []string
			toBeResumed   []string

			currentlySuspended = make(map[string]struct{})
			desiredSuspended   = make(map[string]struct{})
		)

		// Convert the items to a map, so it's easy to create an effective diff from these two slices.
		for _, p := range existingASG.CurrentlySuspendProcesses {
			currentlySuspended[p] = struct{}{}
		}

		for _, p := range suspendedProcessesSlice {
			desiredSuspended[p] = struct{}{}
		}

		// Anything that remains in the desired items is not currently suspended so must be suspended.
		// Anything that remains in the currentlySuspended list must be resumed since they were not part of
		// desiredSuspended.
		for k := range desiredSuspended {
			if _, ok := currentlySuspended[k]; ok {
				delete(desiredSuspended, k)
			}
			delete(currentlySuspended, k)
		}

		// Convert them back into lists to pass them to resume/suspend.
		for k := range desiredSuspended {
			toBeSuspended = append(toBeSuspended, k)
		}

		for k := range currentlySuspended {
			toBeResumed = append(toBeResumed, k)
		}

		if len(toBeSuspended) > 0 {
			clusterScope.Info("suspending processes", "processes", toBeSuspended)
			if err := asgSvc.SuspendProcesses(existingASG.Name, toBeSuspended); err != nil {
				return errors.Wrapf(err, "failed to suspend processes while trying update pool")
			}
		}
		if len(toBeResumed) > 0 {
			clusterScope.Info("resuming processes", "processes", toBeResumed)
			if err := asgSvc.ResumeProcesses(existingASG.Name, toBeResumed); err != nil {
				return errors.Wrapf(err, "failed to resume processes while trying update pool")
			}
		}
	}
	return nil
}

func (r *AWSMachinePoolReconciler) createPool(machinePoolScope *scope.MachinePoolScope, clusterScope cloud.ClusterScoper) error {
	clusterScope.Info("Initializing ASG client")

	asgsvc := r.getASGService(clusterScope)

	machinePoolScope.Info("Creating Autoscaling Group")
	if _, err := asgsvc.CreateASG(machinePoolScope); err != nil {
		return errors.Wrapf(err, "failed to create AWSMachinePool")
	}

	return nil
}

func (r *AWSMachinePoolReconciler) findASG(machinePoolScope *scope.MachinePoolScope, asgsvc services.ASGInterface) (*expinfrav1.AutoScalingGroup, error) {
	// Query the instance using tags.
	asg, err := asgsvc.GetASGByName(machinePoolScope)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to query AWSMachinePool by name")
	}

	return asg, nil
}

// diffASG compares incoming AWSMachinePool and compares against existing ASG.
func diffASG(machinePoolScope *scope.MachinePoolScope, existingASG *expinfrav1.AutoScalingGroup) string {
	detectedMachinePoolSpec := machinePoolScope.MachinePool.Spec.DeepCopy()

	if !annotations.ReplicasManagedByExternalAutoscaler(machinePoolScope.MachinePool) {
		detectedMachinePoolSpec.Replicas = existingASG.DesiredCapacity
	}
	if diff := cmp.Diff(machinePoolScope.MachinePool.Spec, *detectedMachinePoolSpec); diff != "" {
		return diff
	}

	detectedAWSMachinePoolSpec := machinePoolScope.AWSMachinePool.Spec.DeepCopy()
	detectedAWSMachinePoolSpec.MaxSize = existingASG.MaxSize
	detectedAWSMachinePoolSpec.MinSize = existingASG.MinSize
	detectedAWSMachinePoolSpec.CapacityRebalance = existingASG.CapacityRebalance
	{
		mixedInstancesPolicy := machinePoolScope.AWSMachinePool.Spec.MixedInstancesPolicy
		// InstancesDistribution is optional, and the default values come from AWS, so
		// they are not set by the AWSMachinePool defaulting webhook. If InstancesDistribution is
		// not set, we use the AWS values for the purpose of comparison.
		if mixedInstancesPolicy != nil && mixedInstancesPolicy.InstancesDistribution == nil {
			mixedInstancesPolicy = machinePoolScope.AWSMachinePool.Spec.MixedInstancesPolicy.DeepCopy()
			mixedInstancesPolicy.InstancesDistribution = existingASG.MixedInstancesPolicy.InstancesDistribution
		}

		if !cmp.Equal(mixedInstancesPolicy, existingASG.MixedInstancesPolicy) {
			detectedAWSMachinePoolSpec.MixedInstancesPolicy = existingASG.MixedInstancesPolicy
		}
	}

	return cmp.Diff(machinePoolScope.AWSMachinePool.Spec, *detectedAWSMachinePoolSpec)
}

// getOwnerMachinePool returns the MachinePool object owning the current resource.
func getOwnerMachinePool(ctx context.Context, c client.Client, obj metav1.ObjectMeta) (*expclusterv1.MachinePool, error) {
	for _, ref := range obj.OwnerReferences {
		if ref.Kind != "MachinePool" {
			continue
		}
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		if gv.Group == expclusterv1.GroupVersion.Group {
			return getMachinePoolByName(ctx, c, obj.Namespace, ref.Name)
		}
	}
	return nil, nil
}

// getMachinePoolByName finds and return a Machine object using the specified params.
func getMachinePoolByName(ctx context.Context, c client.Client, namespace, name string) (*expclusterv1.MachinePool, error) {
	m := &expclusterv1.MachinePool{}
	key := client.ObjectKey{Name: name, Namespace: namespace}
	if err := c.Get(ctx, key, m); err != nil {
		return nil, err
	}
	return m, nil
}

func machinePoolToInfrastructureMapFunc(gvk schema.GroupVersionKind) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		m, ok := o.(*expclusterv1.MachinePool)
		if !ok {
			klog.Errorf("Expected a MachinePool but got a %T", o)
		}

		gk := gvk.GroupKind()
		// Return early if the GroupKind doesn't match what we expect
		infraGK := m.Spec.Template.Spec.InfrastructureRef.GroupVersionKind().GroupKind()
		if gk != infraGK {
			return nil
		}

		return []reconcile.Request{
			{
				NamespacedName: client.ObjectKey{
					Namespace: m.Namespace,
					Name:      m.Spec.Template.Spec.InfrastructureRef.Name,
				},
			},
		}
	}
}

func (r *AWSMachinePoolReconciler) getInfraCluster(ctx context.Context, log *logger.Logger, cluster *clusterv1.Cluster, awsMachinePool *expinfrav1.AWSMachinePool) (scope.EC2Scope, error) {
	var clusterScope *scope.ClusterScope
	var managedControlPlaneScope *scope.ManagedControlPlaneScope
	var err error

	if cluster.Spec.ControlPlaneRef != nil && cluster.Spec.ControlPlaneRef.Kind == controllers.AWSManagedControlPlaneRefKind {
		controlPlane := &ekscontrolplanev1.AWSManagedControlPlane{}
		controlPlaneName := client.ObjectKey{
			Namespace: awsMachinePool.Namespace,
			Name:      cluster.Spec.ControlPlaneRef.Name,
		}

		if err := r.Get(ctx, controlPlaneName, controlPlane); err != nil {
			// AWSManagedControlPlane is not ready
			return nil, nil //nolint:nilerr
		}

		managedControlPlaneScope, err = scope.NewManagedControlPlaneScope(scope.ManagedControlPlaneScopeParams{
			Client:                       r.Client,
			Logger:                       log,
			Cluster:                      cluster,
			ControlPlane:                 controlPlane,
			ControllerName:               "awsManagedControlPlane",
			TagUnmanagedNetworkResources: r.TagUnmanagedNetworkResources,
		})
		if err != nil {
			return nil, err
		}

		return managedControlPlaneScope, nil
	}

	awsCluster := &infrav1.AWSCluster{}

	infraClusterName := client.ObjectKey{
		Namespace: awsMachinePool.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}

	if err := r.Client.Get(ctx, infraClusterName, awsCluster); err != nil {
		// AWSCluster is not ready
		return nil, nil //nolint:nilerr
	}

	// Create the cluster scope
	clusterScope, err = scope.NewClusterScope(scope.ClusterScopeParams{
		Client:                       r.Client,
		Logger:                       log,
		Cluster:                      cluster,
		AWSCluster:                   awsCluster,
		ControllerName:               "awsmachine",
		TagUnmanagedNetworkResources: r.TagUnmanagedNetworkResources,
	})
	if err != nil {
		return nil, err
	}

	return clusterScope, nil
}
