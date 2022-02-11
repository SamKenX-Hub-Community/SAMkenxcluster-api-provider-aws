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
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/gomega"
	"golang.org/x/net/context"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "sigs.k8s.io/cluster-api-provider-aws/api/v1beta1"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/scope"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services/mock_services"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
)

func TestAWSClusterReconciler_Reconcile(t *testing.T) {
	testCases := []struct {
		name         string
		awsCluster   *infrav1.AWSCluster
		ownerCluster *clusterv1.Cluster
		expectError  bool
	}{
		{
			name:        "Should not reconcile if owner reference is not set",
			awsCluster:  &infrav1.AWSCluster{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("aws-test-%s", util.RandomString(5)), Namespace: fmt.Sprintf("namespace-%s", util.RandomString(5))}},
			expectError: false,
		},
		{
			name: "Should fail Reconcile with GetOwnerCluster failure",
			awsCluster: &infrav1.AWSCluster{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("aws-test-%s", util.RandomString(5)), Namespace: fmt.Sprintf("namespace-%s", util.RandomString(5)), OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "Cluster",
					Name:       "capi-test-cluster",
					UID:        "1",
				}}}},
			expectError: true,
		},
		{
			name: "Should not Reconcile if cluster is paused",
			awsCluster: &infrav1.AWSCluster{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("aws-test-%s", util.RandomString(5)), Namespace: "default", Annotations: map[string]string{clusterv1.PausedAnnotation: ""}, OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "Cluster",
					Name:       "capi-test-cluster",
					UID:        "1",
				},
			}}},
			ownerCluster: &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "capi-test-cluster"}},
			expectError:  false,
		},
		{
			name:        "Should Reconcile successfully if no AWSCluster found",
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()
			reconciler := &AWSClusterReconciler{
				Client: testEnv.Client,
			}

			ns, err := testEnv.CreateNamespace(ctx, fmt.Sprintf("namespace-%s", util.RandomString(5)))
			g.Expect(err).To(BeNil())
			if tc.ownerCluster != nil {
				tc.ownerCluster.Namespace = ns.Name
				g.Expect(testEnv.Create(ctx, tc.ownerCluster)).To(Succeed())
				defer func(do ...client.Object) {
					g.Expect(testEnv.Cleanup(ctx, do...)).To(Succeed())
				}(tc.ownerCluster)
			}

			createCluster(g, ctx, tc.awsCluster, ns.Name)
			defer cleanupCluster(g, ctx, tc.awsCluster, ns)
			if tc.ownerCluster != nil {
				tc.ownerCluster.ObjectMeta.Namespace = ns.Name
			}

			if tc.awsCluster != nil {
				_, err := reconciler.Reconcile(ctx, ctrl.Request{
					NamespacedName: client.ObjectKey{
						Namespace: tc.awsCluster.Namespace,
						Name:      tc.awsCluster.Name,
					},
				})
				if tc.expectError {
					g.Expect(err).ToNot(BeNil())
				} else {
					g.Expect(err).To(BeNil())
				}
			} else {
				_, err := reconciler.Reconcile(ctx, ctrl.Request{
					NamespacedName: client.ObjectKey{
						Namespace: ns.Name,
						Name:      "test",
					},
				})
				g.Expect(err).To(BeNil())
			}
		})
	}
}

func TestAWSClusterReconcileOperations(t *testing.T) {
	var (
		reconciler AWSClusterReconciler
		mockCtrl   *gomock.Controller
		ec2Svc     *mock_services.MockEC2MachineInterface
		elbSvc     *mock_services.MockELBInterface
		networkSvc *mock_services.MockNetworkInterface
		sgSvc      *mock_services.MockSecurityGroupInterface
		recorder   *record.FakeRecorder
	)

	setup := func(t *testing.T, awsCluster *infrav1.AWSCluster) client.WithWatch {
		t.Helper()
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-secret",
				Namespace: "capa-system",
			},
			Data: map[string][]byte{
				"AccessKeyID":     []byte("access-key-id"),
				"SecretAccessKey": []byte("secret-access-key"),
				"SessionToken":    []byte("session-token"),
			},
		}
		csClient := fake.NewClientBuilder().WithObjects(awsCluster, secret).Build()

		mockCtrl = gomock.NewController(t)
		ec2Svc = mock_services.NewMockEC2MachineInterface(mockCtrl)
		elbSvc = mock_services.NewMockELBInterface(mockCtrl)
		networkSvc = mock_services.NewMockNetworkInterface(mockCtrl)
		sgSvc = mock_services.NewMockSecurityGroupInterface(mockCtrl)

		recorder = record.NewFakeRecorder(2)

		reconciler = AWSClusterReconciler{
			Client: csClient,
			ec2ServiceFactory: func(scope.EC2Scope) services.EC2MachineInterface {
				return ec2Svc
			},
			elbServiceFactory: func(elbScope scope.ELBScope) services.ELBInterface {
				return elbSvc
			},
			networkServiceFactory: func(clusterScope scope.ClusterScope) services.NetworkInterface {
				return networkSvc
			},
			securityGroupFactory: func(clusterScope scope.ClusterScope) services.SecurityGroupInterface {
				return sgSvc
			},
			Recorder: recorder,
		}
		return csClient
	}

	teardown := func() {
		mockCtrl.Finish()
	}

	t.Run("Reconciling an AWSCluster", func(t *testing.T) {
		t.Run("Reconcile success", func(t *testing.T) {
			t.Run("Should successfully create AWSCluster with Cluster Finalizer and LoadBalancerReady status true on AWSCluster", func(t *testing.T) {
				g := NewWithT(t)
				runningCluster := func() {
					ec2Svc.EXPECT().ReconcileBastion().Return(nil)
					elbSvc.EXPECT().ReconcileLoadbalancers().Return(nil)
					networkSvc.EXPECT().ReconcileNetwork().Return(nil)
					sgSvc.EXPECT().ReconcileSecurityGroups().Return(nil)
				}

				awsCluster := getAWSCluster("test", "test")
				csClient := setup(t, &awsCluster)
				defer teardown()
				runningCluster()
				cs, err := scope.NewClusterScope(
					scope.ClusterScopeParams{
						Client:     csClient,
						Cluster:    &clusterv1.Cluster{},
						AWSCluster: &awsCluster,
					},
				)
				g.Expect(err).To(BeNil())
				awsCluster.Status.Network.APIServerELB.DNSName = "www.google.com"
				awsCluster.Status.Network.APIServerELB.AvailabilityZones = []string{"us-east-1a", "us-east-1b", "us-east-1c", "us-east-1d", "us-east-1e"}
				cs.SetSubnets(infrav1.Subnets{
					{
						ID:               "private-subnet-1",
						AvailabilityZone: "us-east-1b",
						IsPublic:         false,
					},
					{
						ID:               "private-subnet-2",
						AvailabilityZone: "us-east-1a",
						IsPublic:         false,
					},
					{
						ID:               "private-subnet-3",
						AvailabilityZone: "us-east-1c",
						IsPublic:         false,
					},
					{
						ID:               "private-subnet-4",
						AvailabilityZone: "us-east-1d",
						IsPublic:         false,
					},
					{
						ID:               "private-subnet-5",
						AvailabilityZone: "us-east-1e",
						IsPublic:         false,
					},
				})
				_, err = reconciler.reconcileNormal(cs)
				g.Expect(err).To(BeNil())
				expectAWSClusterConditions(g, cs.AWSCluster, []conditionAssertion{{infrav1.LoadBalancerReadyCondition, corev1.ConditionTrue, "", ""}})
				g.Expect(awsCluster.GetFinalizers()).To(ContainElement(infrav1.ClusterFinalizer))
			})
		})
		t.Run("Reconcile failure", func(t *testing.T) {
			expectedErr := errors.New("failed to get resource")
			t.Run("Should fail AWSCluster create with reconcile network failure", func(t *testing.T) {
				g := NewWithT(t)
				awsCluster := getAWSCluster("test", "test")
				runningCluster := func() {
					networkSvc.EXPECT().ReconcileNetwork().Return(expectedErr)
				}
				csClient := setup(t, &awsCluster)
				defer teardown()
				runningCluster()
				cs, err := scope.NewClusterScope(
					scope.ClusterScopeParams{
						Client:     csClient,
						Cluster:    &clusterv1.Cluster{},
						AWSCluster: &awsCluster,
					},
				)
				g.Expect(err).To(BeNil())
				_, err = reconciler.reconcileNormal(cs)
				g.Expect(err).Should(Equal(expectedErr))
			})
			t.Run("Should fail AWSCluster create with ClusterSecurityGroupsReadyCondition status false", func(t *testing.T) {
				g := NewWithT(t)
				awsCluster := getAWSCluster("test", "test")
				runningCluster := func() {
					networkSvc.EXPECT().ReconcileNetwork().Return(nil)
					sgSvc.EXPECT().ReconcileSecurityGroups().Return(expectedErr)
				}
				csClient := setup(t, &awsCluster)
				defer teardown()
				runningCluster()
				cs, err := scope.NewClusterScope(
					scope.ClusterScopeParams{
						Client:     csClient,
						Cluster:    &clusterv1.Cluster{},
						AWSCluster: &awsCluster,
					},
				)
				g.Expect(err).To(BeNil())
				_, err = reconciler.reconcileNormal(cs)
				g.Expect(err).ToNot(BeNil())
				expectAWSClusterConditions(g, cs.AWSCluster, []conditionAssertion{{infrav1.ClusterSecurityGroupsReadyCondition, corev1.ConditionFalse, clusterv1.ConditionSeverityWarning, infrav1.ClusterSecurityGroupReconciliationFailedReason}})
			})
			t.Run("Should fail AWSCluster create with BastionHostReadyCondition status false", func(t *testing.T) {
				g := NewWithT(t)
				awsCluster := getAWSCluster("test", "test")
				runningCluster := func() {
					networkSvc.EXPECT().ReconcileNetwork().Return(nil)
					sgSvc.EXPECT().ReconcileSecurityGroups().Return(nil)
					ec2Svc.EXPECT().ReconcileBastion().Return(expectedErr)
				}
				csClient := setup(t, &awsCluster)
				defer teardown()
				runningCluster()
				cs, err := scope.NewClusterScope(
					scope.ClusterScopeParams{
						Client:     csClient,
						Cluster:    &clusterv1.Cluster{},
						AWSCluster: &awsCluster,
					},
				)
				g.Expect(err).To(BeNil())
				_, err = reconciler.reconcileNormal(cs)
				g.Expect(err).ToNot(BeNil())
				expectAWSClusterConditions(g, cs.AWSCluster, []conditionAssertion{{infrav1.BastionHostReadyCondition, corev1.ConditionFalse, clusterv1.ConditionSeverityWarning, infrav1.BastionHostFailedReason}})
			})
			t.Run("Should fail AWSCluster create with failure in LoadBalancer reconciliation", func(t *testing.T) {
				g := NewWithT(t)
				awsCluster := getAWSCluster("test", "test")
				runningCluster := func() {
					networkSvc.EXPECT().ReconcileNetwork().Return(nil)
					sgSvc.EXPECT().ReconcileSecurityGroups().Return(nil)
					ec2Svc.EXPECT().ReconcileBastion().Return(nil)
					elbSvc.EXPECT().ReconcileLoadbalancers().Return(expectedErr)
				}
				csClient := setup(t, &awsCluster)
				defer teardown()
				runningCluster()
				cs, err := scope.NewClusterScope(
					scope.ClusterScopeParams{
						Client:     csClient,
						Cluster:    &clusterv1.Cluster{},
						AWSCluster: &awsCluster,
					},
				)
				g.Expect(err).To(BeNil())
				_, err = reconciler.reconcileNormal(cs)
				g.Expect(err).ToNot(BeNil())
				expectAWSClusterConditions(g, cs.AWSCluster, []conditionAssertion{{infrav1.LoadBalancerReadyCondition, corev1.ConditionFalse, clusterv1.ConditionSeverityWarning, infrav1.LoadBalancerFailedReason}})
			})
			t.Run("Should fail AWSCluster create with LoadBalancer reconcile failure with WaitForDNSName condition as false", func(t *testing.T) {
				g := NewWithT(t)
				awsCluster := getAWSCluster("test", "test")
				runningCluster := func() {
					networkSvc.EXPECT().ReconcileNetwork().Return(nil)
					sgSvc.EXPECT().ReconcileSecurityGroups().Return(nil)
					ec2Svc.EXPECT().ReconcileBastion().Return(nil)
					elbSvc.EXPECT().ReconcileLoadbalancers().Return(nil)
				}
				csClient := setup(t, &awsCluster)
				defer teardown()
				runningCluster()
				cs, err := scope.NewClusterScope(
					scope.ClusterScopeParams{
						Client:     csClient,
						Cluster:    &clusterv1.Cluster{},
						AWSCluster: &awsCluster,
					},
				)
				g.Expect(err).To(BeNil())
				_, err = reconciler.reconcileNormal(cs)
				g.Expect(err).To(BeNil())
				expectAWSClusterConditions(g, cs.AWSCluster, []conditionAssertion{{infrav1.LoadBalancerReadyCondition, corev1.ConditionFalse, clusterv1.ConditionSeverityInfo, infrav1.WaitForDNSNameReason}})
			})
			t.Run("Should fail AWSCluster create with LoadBalancer reconcile failure with WaitForDNSNameResolve condition as false", func(t *testing.T) {
				g := NewWithT(t)
				awsCluster := getAWSCluster("test", "test")
				runningCluster := func() {
					networkSvc.EXPECT().ReconcileNetwork().Return(nil)
					sgSvc.EXPECT().ReconcileSecurityGroups().Return(nil)
					ec2Svc.EXPECT().ReconcileBastion().Return(nil)
					elbSvc.EXPECT().ReconcileLoadbalancers().Return(nil)
				}
				csClient := setup(t, &awsCluster)
				defer teardown()
				runningCluster()
				cs, err := scope.NewClusterScope(
					scope.ClusterScopeParams{
						Client:     csClient,
						Cluster:    &clusterv1.Cluster{},
						AWSCluster: &awsCluster,
					},
				)
				awsCluster.Status.Network.APIServerELB.DNSName = "test-apiserver.us-east-1.aws"
				g.Expect(err).To(BeNil())
				_, err = reconciler.reconcileNormal(cs)
				g.Expect(err).To(BeNil())
				expectAWSClusterConditions(g, cs.AWSCluster, []conditionAssertion{{infrav1.LoadBalancerReadyCondition, corev1.ConditionFalse, clusterv1.ConditionSeverityInfo, infrav1.WaitForDNSNameResolveReason}})
			})
		})
	})
	t.Run("Reconcile delete AWSCluster", func(t *testing.T) {
		t.Run("Reconcile success", func(t *testing.T) {
			deleteCluster := func() {
				ec2Svc.EXPECT().DeleteBastion().Return(nil)
				elbSvc.EXPECT().DeleteLoadbalancers().Return(nil)
				networkSvc.EXPECT().DeleteNetwork().Return(nil)
				sgSvc.EXPECT().DeleteSecurityGroups().Return(nil)
			}
			t.Run("Should successfully delete AWSCluster with Cluster Finalizer removed", func(t *testing.T) {
				g := NewWithT(t)
				awsCluster := getAWSCluster("test", "test")
				csClient := setup(t, &awsCluster)
				defer teardown()
				deleteCluster()
				cs, err := scope.NewClusterScope(
					scope.ClusterScopeParams{
						Client:     csClient,
						Cluster:    &clusterv1.Cluster{},
						AWSCluster: &awsCluster,
					},
				)
				g.Expect(err).To(BeNil())
				_, err = reconciler.reconcileDelete(cs)
				g.Expect(err).To(BeNil())
				g.Expect(awsCluster.GetFinalizers()).ToNot(ContainElement(infrav1.ClusterFinalizer))
			})
		})
		t.Run("Reconcile failure", func(t *testing.T) {
			expectedErr := errors.New("failed to get resource")
			t.Run("Should fail AWSCluster delete with LoadBalancer deletion failed and Cluster Finalizer not removed", func(t *testing.T) {
				g := NewWithT(t)
				deleteCluster := func() {
					t.Helper()
					elbSvc.EXPECT().DeleteLoadbalancers().Return(expectedErr)
				}
				awsCluster := getAWSCluster("test", "test")
				awsCluster.Finalizers = []string{infrav1.ClusterFinalizer}
				csClient := setup(t, &awsCluster)
				defer teardown()
				deleteCluster()
				cs, err := scope.NewClusterScope(
					scope.ClusterScopeParams{
						Client:     csClient,
						Cluster:    &clusterv1.Cluster{},
						AWSCluster: &awsCluster,
					},
				)
				g.Expect(err).To(BeNil())
				_, err = reconciler.reconcileDelete(cs)
				g.Expect(err).ToNot(BeNil())
				g.Expect(awsCluster.GetFinalizers()).To(ContainElement(infrav1.ClusterFinalizer))
			})
			t.Run("Should fail AWSCluster delete with Bastion deletion failed and Cluster Finalizer not removed", func(t *testing.T) {
				g := NewWithT(t)
				deleteCluster := func() {
					ec2Svc.EXPECT().DeleteBastion().Return(expectedErr)
					elbSvc.EXPECT().DeleteLoadbalancers().Return(nil)
				}
				awsCluster := getAWSCluster("test", "test")
				awsCluster.Finalizers = []string{infrav1.ClusterFinalizer}
				csClient := setup(t, &awsCluster)
				defer teardown()
				deleteCluster()
				cs, err := scope.NewClusterScope(
					scope.ClusterScopeParams{
						Client:     csClient,
						Cluster:    &clusterv1.Cluster{},
						AWSCluster: &awsCluster,
					},
				)
				g.Expect(err).To(BeNil())
				_, err = reconciler.reconcileDelete(cs)
				g.Expect(err).ToNot(BeNil())
				g.Expect(awsCluster.GetFinalizers()).To(ContainElement(infrav1.ClusterFinalizer))
			})
			t.Run("Should fail AWSCluster delete with security group deletion failed and Cluster Finalizer not removed", func(t *testing.T) {
				g := NewWithT(t)
				deleteCluster := func() {
					ec2Svc.EXPECT().DeleteBastion().Return(nil)
					elbSvc.EXPECT().DeleteLoadbalancers().Return(nil)
					sgSvc.EXPECT().DeleteSecurityGroups().Return(expectedErr)
				}
				awsCluster := getAWSCluster("test", "test")
				awsCluster.Finalizers = []string{infrav1.ClusterFinalizer}
				csClient := setup(t, &awsCluster)
				defer teardown()
				deleteCluster()
				cs, err := scope.NewClusterScope(
					scope.ClusterScopeParams{
						Client:     csClient,
						Cluster:    &clusterv1.Cluster{},
						AWSCluster: &awsCluster,
					},
				)
				g.Expect(err).To(BeNil())
				_, err = reconciler.reconcileDelete(cs)
				g.Expect(err).ToNot(BeNil())
				g.Expect(awsCluster.GetFinalizers()).To(ContainElement(infrav1.ClusterFinalizer))
			})
			t.Run("Should fail AWSCluster delete with network deletion failed and Cluster Finalizer not removed", func(t *testing.T) {
				g := NewWithT(t)
				deleteCluster := func() {
					ec2Svc.EXPECT().DeleteBastion().Return(nil)
					elbSvc.EXPECT().DeleteLoadbalancers().Return(nil)
					sgSvc.EXPECT().DeleteSecurityGroups().Return(nil)
					networkSvc.EXPECT().DeleteNetwork().Return(expectedErr)
				}
				awsCluster := getAWSCluster("test", "test")
				awsCluster.Finalizers = []string{infrav1.ClusterFinalizer}
				csClient := setup(t, &awsCluster)
				defer teardown()
				deleteCluster()
				cs, err := scope.NewClusterScope(
					scope.ClusterScopeParams{
						Client:     csClient,
						Cluster:    &clusterv1.Cluster{},
						AWSCluster: &awsCluster,
					},
				)
				g.Expect(err).To(BeNil())
				_, err = reconciler.reconcileDelete(cs)
				g.Expect(err).ToNot(BeNil())
				g.Expect(awsCluster.GetFinalizers()).To(ContainElement(infrav1.ClusterFinalizer))
			})
		})
	})
}

func TestAWSClusterReconciler_RequeueAWSClusterForUnpausedCluster(t *testing.T) {
	testCases := []struct {
		name         string
		awsCluster   *infrav1.AWSCluster
		ownerCluster *clusterv1.Cluster
		requeue      bool
	}{
		{
			name: "Should create reconcile request successfully",
			awsCluster: &infrav1.AWSCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "success-test", Namespace: "default"}, TypeMeta: metav1.TypeMeta{Kind: "AWSCluster", APIVersion: infrav1.GroupVersion.String()},
			},
			ownerCluster: &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: "capi-test", Namespace: "default"}},
			requeue: true,
		},
		{
			name: "Should not create reconcile request if AWSCluster is externally managed",
			awsCluster: &infrav1.AWSCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "external-managed-test", Annotations: map[string]string{clusterv1.ManagedByAnnotation: "capi-test"}},
				TypeMeta:   metav1.TypeMeta{Kind: "AWSCluster", APIVersion: infrav1.GroupVersion.String()},
			},
			ownerCluster: &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: "capi-test"}},
			requeue: false,
		},
		{
			name:         "Should not create reconcile request for deleted clusters",
			ownerCluster: &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "capi-test", DeletionTimestamp: &metav1.Time{Time: time.Now()}}},
			requeue:      false,
		},
		{
			name:         "Should not create reconcile request if infrastructure ref for AWSCluster on owner cluster is not set",
			ownerCluster: &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "capi-test"}},
			requeue:      false,
		},
		{
			name: "Should not create reconcile request if infrastructure ref type on owner cluster is not AWSCluster",
			ownerCluster: &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "capi-test"}, Spec: clusterv1.ClusterSpec{InfrastructureRef: &corev1.ObjectReference{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       "Cluster",
				Name:       "aws-test"}}},
			requeue: false,
		},
		{
			name: "Should not create reconcile request if AWSCluster not found",
			ownerCluster: &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "capi-test"}, Spec: clusterv1.ClusterSpec{InfrastructureRef: &corev1.ObjectReference{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       "AWSCluster",
				Name:       "aws-test"}}},
			requeue: false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()
			log := ctrl.LoggerFrom(ctx)
			reconciler := &AWSClusterReconciler{
				Client: testEnv.Client,
			}

			ns, err := testEnv.CreateNamespace(ctx, fmt.Sprintf("namespace-%s", util.RandomString(5)))
			g.Expect(err).To(BeNil())

			if tc.ownerCluster != nil {
				if tc.awsCluster != nil {
					tc.ownerCluster.Spec = clusterv1.ClusterSpec{InfrastructureRef: &corev1.ObjectReference{
						APIVersion: infrav1.GroupVersion.String(),
						Kind:       "AWSCluster",
						Name:       tc.awsCluster.ObjectMeta.Name,
						Namespace:  ns.Name,
					}}
				}
				tc.ownerCluster.ObjectMeta.Namespace = ns.Name
			}
			createCluster(g, ctx, tc.awsCluster, ns.Name)
			defer cleanupCluster(g, ctx, tc.awsCluster, ns)
			handlerFunc := reconciler.requeueAWSClusterForUnpausedCluster(ctx, log)
			result := handlerFunc(tc.ownerCluster)
			if tc.requeue {
				g.Expect(result).To(ContainElement(reconcile.Request{
					NamespacedName: types.NamespacedName{
						Namespace: ns.Name,
						Name:      "success-test",
					},
				}))
			} else {
				g.Expect(result).To(BeNil())
			}
		})
	}
}

func createCluster(g *WithT, ctx context.Context, awsCluster *infrav1.AWSCluster, namespace string) {
	if awsCluster != nil {
		awsCluster.ObjectMeta.Namespace = namespace
		awsCluster.Default()
		g.Expect(testEnv.Create(ctx, awsCluster)).To(Succeed())
	}
}

func cleanupCluster(g *WithT, ctx context.Context, awsCluster *infrav1.AWSCluster, namespace *corev1.Namespace) {
	if awsCluster != nil {
		func(do ...client.Object) {
			g.Expect(testEnv.Cleanup(ctx, do...)).To(Succeed())
		}(awsCluster, namespace)
	}
}

func expectAWSClusterConditions(g *WithT, m *infrav1.AWSCluster, expected []conditionAssertion) {
	g.Expect(len(m.Status.Conditions)).To(BeNumerically(">=", len(expected)), "number of conditions")
	for _, c := range expected {
		actual := conditions.Get(m, c.conditionType)
		g.Expect(actual).To(Not(BeNil()))
		g.Expect(actual.Type).To(Equal(c.conditionType))
		g.Expect(actual.Status).To(Equal(c.status))
		g.Expect(actual.Severity).To(Equal(c.severity))
		g.Expect(actual.Reason).To(Equal(c.reason))
	}
}

func getAWSCluster(name, namespace string) infrav1.AWSCluster {
	return infrav1.AWSCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: infrav1.AWSClusterSpec{
			Region: "us-east-1",
			NetworkSpec: infrav1.NetworkSpec{
				VPC: infrav1.VPCSpec{
					ID:        "vpc-exists",
					CidrBlock: "10.0.0.0/8",
				},
				Subnets: infrav1.Subnets{
					{
						ID:               "subnet-1",
						AvailabilityZone: "us-east-1a",
						CidrBlock:        "10.0.10.0/24",
						IsPublic:         false,
					},
					{
						ID:               "subnet-2",
						AvailabilityZone: "us-east-1c",
						CidrBlock:        "10.0.11.0/24",
						IsPublic:         true,
					},
				},
				SecurityGroupOverrides: map[infrav1.SecurityGroupRole]string{},
			},
			Bastion: infrav1.Bastion{Enabled: true},
		},
	}
}