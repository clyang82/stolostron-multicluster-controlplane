package e2e_test

import (
	"fmt"
	"os"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"
	clusterinfov1beta1 "github.com/stolostron/cluster-lifecycle-api/clusterinfo/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	policyv1 "open-cluster-management.io/governance-policy-propagator/api/v1"

	"github.com/stolostron/multicluster-controlplane/test/e2e/util"
)

const (
	policyName      = "policy-limitrange"
	policyNamespace = "default"
	limitrangeName  = "container-mem-limit-range"
)

var _ = ginkgo.Describe("hosted mode loopback test", ginkgo.Ordered, func() {
	ginkgo.BeforeEach(func() {
		ginkgo.By(fmt.Sprintf("Wait for klusterlet %s available", hostedManagedClusterName), func() {
			gomega.Eventually(func() error {
				klusterlet, err := klusterletClient.Get(ctx, hostedManagedClusterName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if !meta.IsStatusConditionTrue(klusterlet.Status.Conditions, "Available") {
					return fmt.Errorf("expected %s is available, but failed", hostedManagedClusterName)
				}
				if !meta.IsStatusConditionTrue(klusterlet.Status.Conditions, "Applied") {
					return fmt.Errorf("expected %s is available, but failed", hostedManagedClusterName)
				}
				if meta.IsStatusConditionTrue(klusterlet.Status.Conditions, "HubConnectionDegraded") {
					return fmt.Errorf("expected %s is available, but failed", hostedManagedClusterName)
				}
				if meta.IsStatusConditionTrue(klusterlet.Status.Conditions, "AgentDesiredDegraded") {
					return fmt.Errorf("expected %s is available, but failed", hostedManagedClusterName)
				}
				return nil
			}).WithTimeout(timeout).ShouldNot(gomega.HaveOccurred())
		})

		ginkgo.By(fmt.Sprintf("Wait for hosted managed cluster %s available", hostedManagedClusterName), func() {
			gomega.Eventually(func() error {
				hostedCluster, err := clusterClient.ClusterV1().ManagedClusters().Get(ctx, hostedManagedClusterName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if !meta.IsStatusConditionTrue(hostedCluster.Status.Conditions, clusterv1.ManagedClusterConditionAvailable) {
					return fmt.Errorf("expected %s is available, but failed", hostedManagedClusterName)
				}

				return nil
			}).WithTimeout(timeout).ShouldNot(gomega.HaveOccurred())
		})
	})

	ginkgo.AfterAll(func() {
		ginkgo.By("Ensure the resources are cleaned on the management cluster", func() {
			gomega.Eventually(func() error {
				deploy := fmt.Sprintf("%s-multicluster-controlplane-agent", hostedManagedClusterName)
				_, err := managementKubeClient.AppsV1().Deployments(controlPlaneNamespace).Get(ctx, deploy, metav1.GetOptions{})
				if err != nil && !errors.IsNotFound(err) {
					return err
				}
				if err == nil {
					return fmt.Errorf("the deploy %s/%s still exists", controlPlaneNamespace, deploy)
				}

				hubSecret := fmt.Sprintf("%s-hub-kubeconfig-secret", hostedManagedClusterName)
				_, err = managementKubeClient.CoreV1().Secrets(controlPlaneNamespace).Get(ctx, hubSecret, metav1.GetOptions{})
				if err != nil && !errors.IsNotFound(err) {
					return err
				}
				if err == nil {
					return fmt.Errorf("the hub secrect %s/%s still exists", controlPlaneNamespace, hubSecret)
				}

				externalSecret := fmt.Sprintf("%s-external-managedcluster-kubeconfig", hostedManagedClusterName)
				_, err = managementKubeClient.CoreV1().Secrets(controlPlaneNamespace).Get(ctx, externalSecret, metav1.GetOptions{})
				if err != nil && !errors.IsNotFound(err) {
					return err
				}
				if err == nil {
					return fmt.Errorf("the external secrect %s/%s still exists", controlPlaneNamespace, externalSecret)
				}

				bootstrapSecret := "multicluster-controlplane-svc-kubeconfig"
				_, err = managementKubeClient.CoreV1().Secrets(controlPlaneNamespace).Get(ctx, bootstrapSecret, metav1.GetOptions{})
				if err != nil {
					return err
				}

				return nil
			}).WithTimeout(timeout).ShouldNot(gomega.HaveOccurred())
		})

		ginkgo.By("Ensure the resources are cleaned on the managed cluster", func() {
			gomega.Eventually(func() error {
				for _, name := range []string{
					"appliedmanifestworks.work.open-cluster-management.io",
					"clusterclaims.cluster.open-cluster-management.io",
				} {
					crd, err := hostedCRDsClient.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if _, ok := crd.Annotations["operator.open-cluster-management.io/version"]; ok {
						return fmt.Errorf("the crd %s version annotation still exists", name)
					}
				}

				return nil
			}).WithTimeout(timeout).ShouldNot(gomega.HaveOccurred())
		})

		ginkgo.By("Delete the hosted managed cluster namespace from controlplane", func() {
			err := kubeClient.CoreV1().Namespaces().Delete(ctx, hostedManagedClusterName, metav1.DeleteOptions{})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			gomega.Eventually(func() error {
				_, err := kubeClient.CoreV1().Namespaces().Get(ctx, hostedManagedClusterName, metav1.GetOptions{})
				if err != nil && !errors.IsNotFound(err) {
					return err
				}
				if err == nil {
					return fmt.Errorf("the managed cluster namespace %s still exists", hostedManagedClusterName)
				}
				return nil
			}).WithTimeout(timeout).ShouldNot(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("work manager should work fine", func() {
		ginkgo.It("should have a synced clusterinfo", func() {
			gomega.Eventually(func() error {
				clusterInfo, err := util.GetResource(ctx, dynamicClient, util.ClusterInfoGVR, hostedManagedClusterName, hostedManagedClusterName)
				if err != nil {
					return err
				}

				if !util.IsResourceStatusConditionTrue(clusterInfo, clusterinfov1beta1.ManagedClusterInfoSynced) {
					return fmt.Errorf("expected clusterinfo %s is synced, but failed", hostedManagedClusterName)
				}

				return nil
			}).WithTimeout(timeout).ShouldNot(gomega.HaveOccurred())
		})

		ginkgo.It("should have required claims", func() {
			gomega.Eventually(func() error {
				if _, err := hostedSpokeClusterClient.ClusterV1alpha1().ClusterClaims().Get(ctx, util.IDClaim, metav1.GetOptions{}); err != nil {
					return err
				}

				if _, err := hostedSpokeClusterClient.ClusterV1alpha1().ClusterClaims().Get(ctx, util.VersionClaim, metav1.GetOptions{}); err != nil {
					return err
				}

				claimNames, err := util.GetManagedClusterClaims(ctx, clusterClient, hostedManagedClusterName)
				if err != nil {
					return err
				}

				if !claimNames.Has(util.IDClaim) {
					return fmt.Errorf("claim %q is not reported", util.IDClaim)
				}

				if !claimNames.Has(util.VersionClaim) {
					return fmt.Errorf("claim %q is not reported", util.VersionClaim)
				}

				return nil
			}).WithTimeout(timeout).ShouldNot(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("policy should work fine", func() {
		ginkgo.It("should be able to propagate policies", func() {
			ginkgo.By("Apply clusterset label for hostedCluster", func() {
				gomega.Eventually(func() error {
					patch := []byte("{\"metadata\": {\"labels\": {\"cluster.open-cluster-management.io/clusterset\": \"clusterset1\"}}}")
					_, err := clusterClient.ClusterV1().ManagedClusters().Patch(ctx,
						hostedManagedClusterName, types.MergePatchType, patch, metav1.PatchOptions{})
					if err != nil {
						return err
					}
					return nil
				}).WithTimeout(timeout).ShouldNot(gomega.HaveOccurred())
			})

			ginkgo.By("Apply policy to the controlplane", func() {
				_, err := util.Kubectl(os.Getenv("CONTROLPLANE_KUBECONFIG"), "apply", "-f", "./test/e2e/testdata/limitrange-policy-placement.yaml")
				if err != nil {
					fmt.Print(err)
				}
				gomega.Expect(err).Should(gomega.Succeed())
			})

			ginkgo.By("Verify the policy is propagated to the managed cluster", func() {
				gomega.Eventually(func() error {
					_, err := managementDynamicClient.Resource(policyv1.GroupVersion.WithResource("policies")).
						Namespace(hostedManagedClusterName).Get(ctx, policyNamespace+"."+policyName, metav1.GetOptions{})
					if err != nil {
						return err
					}
					return nil
				}).WithTimeout(timeout).ShouldNot(gomega.HaveOccurred())
			})

			ginkgo.By("Enforce the policy to the managed cluster", func() {
				gomega.Eventually(func() error {
					policy, err := dynamicClient.Resource(policyv1.GroupVersion.WithResource("policies")).
						Namespace("default").Get(ctx, "policy-limitrange", metav1.GetOptions{})
					if err != nil {
						return err
					}

					policy.Object["spec"].(map[string]interface{})["remediationAction"] = "enforce"
					_, err = dynamicClient.Resource(policyv1.GroupVersion.WithResource("policies")).
						Namespace(policyNamespace).Update(ctx, policy, metav1.UpdateOptions{})
					if err != nil {
						return err
					}
					return nil
				}).WithTimeout(timeout).ShouldNot(gomega.HaveOccurred())
			})

			ginkgo.By("Verify the policy is compliant", func() {
				gomega.Eventually(func() error {
					var err error
					policy, err := managementDynamicClient.Resource(policyv1.GroupVersion.WithResource("policies")).
						Namespace(hostedManagedClusterName).Get(ctx, policyNamespace+"."+policyName, metav1.GetOptions{})
					if err != nil {
						return err
					}

					statusObj, ok := policy.Object["status"]
					if ok {
						status := statusObj.(map[string]interface{})
						if status["compliant"] == "Compliant" {
							return nil
						}
					}
					return fmt.Errorf("policy is not compliant")
				}).WithTimeout(timeout).ShouldNot(gomega.HaveOccurred())
			})

			ginkgo.By("Verify the policy is enforced to the managed cluster", func() {
				gomega.Eventually(func() error {
					_, err := hostedSpokeKubeClient.CoreV1().LimitRanges(policyNamespace).
						Get(ctx, limitrangeName, metav1.GetOptions{})
					if err != nil {
						return err
					}
					return nil
				}).WithTimeout(timeout).ShouldNot(gomega.HaveOccurred())
			})
		})
	})

	ginkgo.Context("delete the hosted managed cluster", func() {
		ginkgo.It("should delete the klusterlet", func() {
			err := klusterletClient.Delete(ctx, hostedManagedClusterName, metav1.DeleteOptions{})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			gomega.Eventually(func() error {
				_, err := klusterletClient.Get(ctx, hostedManagedClusterName, metav1.GetOptions{})
				if err != nil && !errors.IsNotFound(err) {
					return err
				}
				if err == nil {
					return fmt.Errorf("the klusterlet %s still exists", hostedManagedClusterName)
				}
				return nil
			}).WithTimeout(timeout).ShouldNot(gomega.HaveOccurred())
		})
	})
})
