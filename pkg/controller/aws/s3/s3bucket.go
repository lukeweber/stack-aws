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

package s3

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/service/iam"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	bucketv1alpha2 "github.com/crossplaneio/stack-aws/aws/apis/storage/v1alpha2"
	awsv1alpha2 "github.com/crossplaneio/stack-aws/aws/apis/v1alpha2"
	"github.com/crossplaneio/stack-aws/pkg/clients/aws"
	"github.com/crossplaneio/stack-aws/pkg/clients/aws/s3"

	runtimev1alpha1 "github.com/crossplaneio/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplaneio/crossplane-runtime/pkg/logging"
	"github.com/crossplaneio/crossplane-runtime/pkg/meta"
	"github.com/crossplaneio/crossplane-runtime/pkg/resource"
	"github.com/crossplaneio/crossplane-runtime/pkg/util"
)

const (
	controllerName = "s3bucket.aws.crossplane.io"
	finalizer      = "finalizer." + controllerName
)

var (
	log           = logging.Logger.WithName("controller." + controllerName)
	ctx           = context.Background()
	result        = reconcile.Result{}
	resultRequeue = reconcile.Result{Requeue: true}
)

// Reconciler reconciles a S3Bucket object
type Reconciler struct {
	client.Client
	scheme     *runtime.Scheme
	kubeclient kubernetes.Interface
	recorder   record.EventRecorder

	connect func(*bucketv1alpha2.S3Bucket) (s3.Service, error)
	create  func(*bucketv1alpha2.S3Bucket, s3.Service) (reconcile.Result, error)
	sync    func(*bucketv1alpha2.S3Bucket, s3.Service) (reconcile.Result, error)
	delete  func(*bucketv1alpha2.S3Bucket, s3.Service) (reconcile.Result, error)
}

// BucketController is responsible for adding the Bucket controller and its
// corresponding reconciler to the manager with any runtime configuration.
type BucketController struct{}

// SetupWithManager creates a newSyncDeleter Controller and adds it to the Manager with default RBAC.
// The Manager will set fields on the Controller and Start it when the Manager is Started.
func (c *BucketController) SetupWithManager(mgr ctrl.Manager) error {
	r := &Reconciler{
		Client:     mgr.GetClient(),
		scheme:     mgr.GetScheme(),
		kubeclient: kubernetes.NewForConfigOrDie(mgr.GetConfig()),
		recorder:   mgr.GetEventRecorderFor(controllerName),
	}
	r.connect = r._connect
	r.create = r._create
	r.delete = r._delete
	r.sync = r._sync

	return ctrl.NewControllerManagedBy(mgr).
		Named(controllerName).
		For(&bucketv1alpha2.S3Bucket{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}

// fail - helper function to set fail condition with reason and message
func (r *Reconciler) fail(bucket *bucketv1alpha2.S3Bucket, err error) (reconcile.Result, error) {
	bucket.Status.SetConditions(runtimev1alpha1.ReconcileError(err))
	return reconcile.Result{Requeue: true}, r.Update(context.TODO(), bucket)
}

// connectionSecret return secret object for this resource
func connectionSecret(bucket *bucketv1alpha2.S3Bucket, accessKey *iam.AccessKey) *corev1.Secret {
	s := resource.ConnectionSecretFor(bucket, bucketv1alpha2.S3BucketGroupVersionKind)
	s.Data = map[string][]byte{
		runtimev1alpha1.ResourceCredentialsSecretUserKey:     []byte(util.StringValue(accessKey.AccessKeyId)),
		runtimev1alpha1.ResourceCredentialsSecretPasswordKey: []byte(util.StringValue(accessKey.SecretAccessKey)),
		runtimev1alpha1.ResourceCredentialsSecretEndpointKey: []byte(bucket.Spec.Region),
	}
	return s
}

func (r *Reconciler) _connect(instance *bucketv1alpha2.S3Bucket) (s3.Service, error) {
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

	// Bucket Region and client region must match.
	config.Region = instance.Spec.Region

	// Create new S3 S3Client
	return s3.NewClient(config), nil
}

func (r *Reconciler) _create(bucket *bucketv1alpha2.S3Bucket, client s3.Service) (reconcile.Result, error) {
	bucket.Status.SetConditions(runtimev1alpha1.Creating(), runtimev1alpha1.ReconcileSuccess())
	meta.AddFinalizer(bucket, finalizer)
	err := client.CreateOrUpdateBucket(bucket)
	if err != nil {
		return r.fail(bucket, err)
	}

	// Set username for iam user
	if bucket.Status.IAMUsername == "" {
		bucket.Status.IAMUsername = s3.GenerateBucketUsername(bucket)
	}

	// Get access keys for iam user
	accessKeys, currentVersion, err := client.CreateUser(bucket.Status.IAMUsername, bucket)
	if err != nil {
		return r.fail(bucket, err)
	}

	// Set user policy version in status so we can detect policy drift
	err = bucket.SetUserPolicyVersion(currentVersion)
	if err != nil {
		return r.fail(bucket, err)
	}

	// Set access keys into a secret for local access creds to s3 bucket
	secret := connectionSecret(bucket, accessKeys)

	_, err = util.ApplySecret(r.kubeclient, secret)
	if err != nil {
		return r.fail(bucket, err)
	}

	// No longer creating, we're ready!
	bucket.Status.SetConditions(runtimev1alpha1.Available())
	resource.SetBindable(bucket)
	return result, r.Update(ctx, bucket)
}

func (r *Reconciler) _sync(bucket *bucketv1alpha2.S3Bucket, client s3.Service) (reconcile.Result, error) {
	if bucket.Status.IAMUsername == "" {
		return r.fail(bucket, errors.New("username not set, .Status.IAMUsername"))
	}
	bucketInfo, err := client.GetBucketInfo(bucket.Status.IAMUsername, bucket)
	if err != nil {
		return r.fail(bucket, err)
	}

	if bucketInfo.Versioning != bucket.Spec.Versioning {
		err := client.UpdateVersioning(bucket)
		if err != nil {
			return r.fail(bucket, err)
		}
	}

	// TODO: Detect if the bucket CannedACL has changed, possibly by managing grants list directly.
	err = client.UpdateBucketACL(bucket)
	if err != nil {
		return r.fail(bucket, err)
	}

	// Eventually consistent, so we check if this version is newer than our stored version.
	changed, err := bucket.HasPolicyChanged(bucketInfo.UserPolicyVersion)
	if err != nil {
		return r.fail(bucket, err)
	}
	if changed {
		currentVersion, err := client.UpdatePolicyDocument(bucket.Status.IAMUsername, bucket)
		if err != nil {
			return r.fail(bucket, err)
		}
		err = bucket.SetUserPolicyVersion(currentVersion)
		if err != nil {
			return r.fail(bucket, err)
		}
	}

	bucket.Status.SetConditions(runtimev1alpha1.ReconcileSuccess())
	return result, r.Update(ctx, bucket)
}

func (r *Reconciler) _delete(bucket *bucketv1alpha2.S3Bucket, client s3.Service) (reconcile.Result, error) {
	bucket.Status.SetConditions(runtimev1alpha1.Deleting(), runtimev1alpha1.ReconcileSuccess())
	if bucket.Spec.ReclaimPolicy == runtimev1alpha1.ReclaimDelete {
		if err := client.DeleteBucket(bucket); err != nil {
			return r.fail(bucket, err)
		}
	}

	meta.RemoveFinalizer(bucket, finalizer)
	return result, r.Update(ctx, bucket)
}

// Reconcile reads that state of the bucket for an Instance object and makes changes based on the state read
// and what is in the Instance.Spec
func (r *Reconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	log.V(logging.Debug).Info("reconciling", "kind", bucketv1alpha2.S3BucketKindAPIVersion, "request", request)

	// Fetch the CRD instance
	bucket := &bucketv1alpha2.S3Bucket{}

	err := r.Get(ctx, request.NamespacedName, bucket)
	if err != nil {
		if kerrors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return result, nil
		}
		log.Error(err, "failed to get object at start of reconcile loop")
		return result, err
	}

	s3Client, err := r.connect(bucket)
	if err != nil {
		return r.fail(bucket, err)
	}

	// Check for deletion
	if bucket.DeletionTimestamp != nil {
		return r.delete(bucket, s3Client)
	}

	// Create s3 bucket
	if bucket.Status.IAMUsername == "" {
		return r.create(bucket, s3Client)
	}

	// Update the bucket if it's no longer there.
	return r.sync(bucket, s3Client)
}
