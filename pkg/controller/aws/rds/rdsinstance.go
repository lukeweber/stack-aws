/*
Copyright 2019 The Crossplane Authors.

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

package rds

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha2 "github.com/crossplaneio/stack-aws/aws/apis/database/v1alpha2"
	awsv1alpha2 "github.com/crossplaneio/stack-aws/aws/apis/v1alpha2"
	"github.com/crossplaneio/stack-aws/pkg/clients/aws"
	"github.com/crossplaneio/stack-aws/pkg/clients/aws/rds"

	runtimev1alpha1 "github.com/crossplaneio/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplaneio/crossplane-runtime/pkg/logging"
	"github.com/crossplaneio/crossplane-runtime/pkg/meta"
	"github.com/crossplaneio/crossplane-runtime/pkg/resource"
	"github.com/crossplaneio/crossplane-runtime/pkg/util"
)

const (
	controllerName = "rds.aws.crossplane.io"
	finalizer      = "finalizer." + controllerName
)

var (
	log           = logging.Logger.WithName("controller." + controllerName)
	ctx           = context.Background()
	result        = reconcile.Result{}
	resultRequeue = reconcile.Result{Requeue: true}
)

// Reconciler reconciles a Instance object
type Reconciler struct {
	client.Client
	scheme     *runtime.Scheme
	kubeclient kubernetes.Interface
	recorder   record.EventRecorder

	connect func(*databasev1alpha2.RDSInstance) (rds.Client, error)
	create  func(*databasev1alpha2.RDSInstance, rds.Client) (reconcile.Result, error)
	sync    func(*databasev1alpha2.RDSInstance, rds.Client) (reconcile.Result, error)
	delete  func(*databasev1alpha2.RDSInstance, rds.Client) (reconcile.Result, error)
}

// InstanceController is responsible for adding the RDSInstance
// controller and its corresponding reconciler to the manager with any runtime configuration.
type InstanceController struct{}

// SetupWithManager creates a new Controller and adds it to the Manager with default RBAC. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func (c *InstanceController) SetupWithManager(mgr ctrl.Manager) error {
	r := &Reconciler{
		Client:     mgr.GetClient(),
		scheme:     mgr.GetScheme(),
		kubeclient: kubernetes.NewForConfigOrDie(mgr.GetConfig()),
		recorder:   mgr.GetEventRecorderFor(controllerName),
	}
	r.connect = r._connect
	r.create = r._create
	r.sync = r._sync
	r.delete = r._delete

	return ctrl.NewControllerManagedBy(mgr).
		Named("instance-controller").
		For(&databasev1alpha2.RDSInstance{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}

// fail - helper function to set fail condition with reason and message
func (r *Reconciler) fail(instance *databasev1alpha2.RDSInstance, err error) (reconcile.Result, error) {
	instance.Status.SetConditions(runtimev1alpha1.ReconcileError(err))
	return reconcile.Result{Requeue: true}, r.Update(context.TODO(), instance)
}

// connectionSecret return secret object for this resource
func connectionSecret(instance *databasev1alpha2.RDSInstance, password string) *corev1.Secret {
	s := resource.ConnectionSecretFor(instance, databasev1alpha2.RDSInstanceGroupVersionKind)
	s.Data = map[string][]byte{
		runtimev1alpha1.ResourceCredentialsSecretUserKey:     []byte(instance.Spec.MasterUsername),
		runtimev1alpha1.ResourceCredentialsSecretPasswordKey: []byte(password),
	}
	return s
}

func (r *Reconciler) _connect(instance *databasev1alpha2.RDSInstance) (rds.Client, error) {
	// Fetch AWS Provider
	p := &awsv1alpha2.Provider{}
	err := r.Get(ctx, meta.NamespacedNameOf(instance.Spec.ProviderReference), p)
	if err != nil {
		return nil, err
	}

	// Get Provider's AWS Config
	config, err := aws.Config(r.kubeclient, p)
	if err != nil {
		return nil, err
	}

	// Create new RDS RDSClient
	return rds.NewClient(config), nil
}

func (r *Reconciler) _create(instance *databasev1alpha2.RDSInstance, client rds.Client) (reconcile.Result, error) {
	instance.Status.SetConditions(runtimev1alpha1.Creating())
	resourceName := fmt.Sprintf("%s-%s", instance.Spec.Engine, instance.UID)

	// generate new password
	password, err := util.GeneratePassword(20)
	if err != nil {
		return r.fail(instance, err)
	}

	_, err = util.ApplySecret(r.kubeclient, connectionSecret(instance, password))
	if err != nil {
		return r.fail(instance, err)
	}

	// Create DB Instance
	_, err = client.CreateInstance(resourceName, password, &instance.Spec)
	if err != nil && !rds.IsErrorAlreadyExists(err) {
		return r.fail(instance, err)
	}

	instance.Status.InstanceName = resourceName
	meta.AddFinalizer(instance, finalizer)
	instance.Status.SetConditions(runtimev1alpha1.ReconcileSuccess())

	return resultRequeue, r.Update(ctx, instance)
}

func (r *Reconciler) _sync(instance *databasev1alpha2.RDSInstance, client rds.Client) (reconcile.Result, error) {
	// Search for the RDS instance in AWS
	db, err := client.GetInstance(instance.Status.InstanceName)
	if err != nil {
		return r.fail(instance, err)
	}

	instance.Status.State = db.Status

	switch db.Status {
	case string(databasev1alpha2.RDSInstanceStateCreating):
		instance.Status.SetConditions(runtimev1alpha1.Creating(), runtimev1alpha1.ReconcileSuccess())
		return resultRequeue, r.Update(ctx, instance)
	case string(databasev1alpha2.RDSInstanceStateFailed):
		instance.Status.SetConditions(runtimev1alpha1.Unavailable(), runtimev1alpha1.ReconcileSuccess())
		return result, r.Update(ctx, instance)
	case string(databasev1alpha2.RDSInstanceStateAvailable):
		instance.Status.SetConditions(runtimev1alpha1.Available())
		resource.SetBindable(instance)
	default:
		return r.fail(instance, errors.Errorf("unexpected resource status: %s", db.Status))
	}

	// Retrieve connection secret that was created during resource create phase
	connSecret, err := r.kubeclient.CoreV1().
		Secrets(instance.GetNamespace()).
		Get(instance.GetWriteConnectionSecretToReference().Name, metav1.GetOptions{})
	if err != nil {
		return r.fail(instance, err)
	}

	// Save resource endpoint
	instance.Status.Endpoint = db.Endpoint
	instance.Status.ProviderID = db.ARN

	// Update resource secret
	connSecret.Data[runtimev1alpha1.ResourceCredentialsSecretEndpointKey] = []byte(db.Endpoint)
	_, err = util.ApplySecret(r.kubeclient, connSecret)
	if err != nil {
		return r.fail(instance, err)
	}

	instance.Status.SetConditions(runtimev1alpha1.ReconcileSuccess())
	return result, r.Update(ctx, instance)
}

func (r *Reconciler) _delete(instance *databasev1alpha2.RDSInstance, client rds.Client) (reconcile.Result, error) {
	instance.Status.SetConditions(runtimev1alpha1.Deleting())

	if instance.Spec.ReclaimPolicy == runtimev1alpha1.ReclaimDelete {
		if _, err := client.DeleteInstance(instance.Status.InstanceName); err != nil && !rds.IsErrorNotFound(err) {
			return r.fail(instance, err)
		}
	}

	meta.RemoveFinalizer(instance, finalizer)
	instance.Status.SetConditions(runtimev1alpha1.ReconcileSuccess())
	return result, r.Update(ctx, instance)
}

// Reconcile reads that state of the cluster for a Instance object and makes changes based on the state read
// and what is in the Instance.Spec
func (r *Reconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	log.V(logging.Debug).Info("reconciling", "kind", databasev1alpha2.RDSInstanceKindAPIVersion, "request", request)
	// Fetch the CRD instance
	instance := &databasev1alpha2.RDSInstance{}

	err := r.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		if kerrors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return reconcile.Result{}, nil
		}
		log.Error(err, "failed to get object at start of reconcile loop")
		return reconcile.Result{}, err
	}

	rdsClient, err := r.connect(instance)
	if err != nil {
		return r.fail(instance, err)
	}

	// Check for deletion
	if instance.DeletionTimestamp != nil {
		return r.delete(instance, rdsClient)
	}

	// Create cluster instance
	if instance.Status.InstanceName == "" {
		return r.create(instance, rdsClient)
	}

	// Sync cluster instance status with cluster status
	return r.sync(instance, rdsClient)
}
