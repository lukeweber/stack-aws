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

package v1alpha2

import (
	"testing"

	"github.com/onsi/gomega"
	. "github.com/onsi/gomega"
	"golang.org/x/net/context"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	runtimev1alpha1 "github.com/crossplaneio/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplaneio/crossplane-runtime/pkg/resource"
	"github.com/crossplaneio/crossplane-runtime/pkg/test"

	storagev1alpha1 "github.com/crossplaneio/crossplane/apis/storage/v1alpha1"

	localtest "github.com/crossplaneio/stack-aws/pkg/test"
)

const (
	namespace = "default"
	name      = "test-instance"
)

var (
	c client.Client
)

var _ resource.Managed = &S3Bucket{}

func TestMain(m *testing.M) {
	t := test.NewEnv(namespace, SchemeBuilder.SchemeBuilder, localtest.CRDs())
	c = t.StartClient()
	t.StopAndExit(m.Run())
}

func TestStorageS3Bucket(t *testing.T) {
	key := types.NamespacedName{Name: name, Namespace: namespace}
	perm := storagev1alpha1.ReadOnlyPermission
	created := &S3Bucket{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: S3BucketSpec{
			ResourceSpec: runtimev1alpha1.ResourceSpec{
				ProviderReference: &core.ObjectReference{},
			},
			S3BucketParameters: S3BucketParameters{
				NameFormat:      "test-bucket-name-%s",
				Region:          "us-west-1",
				LocalPermission: &perm,
			},
		},
	}
	g := gomega.NewGomegaWithT(t)

	// Test Create
	fetched := &S3Bucket{}
	g.Expect(c.Create(context.TODO(), created)).NotTo(HaveOccurred())

	g.Expect(c.Get(context.TODO(), key, fetched)).NotTo(HaveOccurred())
	g.Expect(fetched).To(Equal(created))

	// Test Updating the Labels
	updated := fetched.DeepCopy()
	updated.Labels = map[string]string{"hello": "world"}
	g.Expect(c.Update(context.TODO(), updated)).NotTo(HaveOccurred())

	g.Expect(c.Get(context.TODO(), key, fetched)).NotTo(HaveOccurred())
	g.Expect(fetched).To(Equal(updated))

	// Test Delete
	g.Expect(c.Delete(context.TODO(), fetched)).NotTo(HaveOccurred())
	g.Expect(c.Get(context.TODO(), key, fetched)).To(HaveOccurred())
}
