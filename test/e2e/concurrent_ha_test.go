//go:build e2e
// +build e2e

/*
Copyright 2026.

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

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/openshift/external-secrets-operator/api/v1alpha1"
	"github.com/openshift/external-secrets-operator/pkg/controller/common"
	externalsecrets "github.com/openshift/external-secrets-operator/pkg/controller/external_secrets"
	"github.com/openshift/external-secrets-operator/test/utils"
)

// webhookServiceName matches the operator-managed Service for the webhook
// (bindata service_external-secrets-webhook.yml, same name as the Deployment).
const webhookServiceName = externalsecrets.OperandWebhookDeployment

var _ = Describe("Component Replicas and Advanced Overrides", Ordered, Label("Platform:Generic", "Feature:Replicas", "Feature:AdvancedOverrides", "Feature:LeaderElection"), func() {
	var (
		ctx                    context.Context
		clientset              *kubernetes.Clientset
		dynamicClient          = suiteDynamicClient
		runtimeClient          = suiteRuntimeClient
		originalComponentConfigs []operatorv1alpha1.ComponentConfig
	)

	BeforeAll(func() {
		ctx = context.Background()
		clientset = suiteClientset
		Expect(clientset).NotTo(BeNil())
		Expect(runtimeClient).NotTo(BeNil())
		Expect(dynamicClient).NotTo(BeNil())

		By("Ensuring ExternalSecretsConfig is Ready")
		Expect(ensureExternalSecretsConfigReady(ctx)).To(Succeed())

		By("Capturing original componentConfigs for restoration")
		esc := &operatorv1alpha1.ExternalSecretsConfig{}
		Expect(runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, esc)).To(Succeed())
		originalComponentConfigs = esc.Spec.ControllerConfig.ComponentConfigs

		By("Waiting for operand pods to be ready")
		Expect(utils.VerifyOperandPodsReady(ctx, clientset, operandNamespace, esc)).To(Succeed())
	})

	AfterAll(func() {
		By("Restoring original componentConfigs")
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			esc := &operatorv1alpha1.ExternalSecretsConfig{}
			if err := runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, esc); err != nil {
				return err
			}
			esc.Spec.ControllerConfig.ComponentConfigs = originalComponentConfigs
			return runtimeClient.Update(ctx, esc)
		})).To(Succeed())
		Expect(utils.WaitForExternalSecretsConfigReady(ctx, dynamicClient, common.ExternalSecretsConfigObjectName, 3*time.Minute)).To(Succeed())

		esc := &operatorv1alpha1.ExternalSecretsConfig{}
		Expect(runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, esc)).To(Succeed())
		By("Waiting for operand pods to be ready after cleanup")
		Expect(utils.VerifyOperandPodsReady(ctx, clientset, operandNamespace, esc)).To(Succeed())
	})

	Context("Core controller HA lifecycle", Ordered, Label("Feature:Replicas", "Feature:LeaderElection"), func() {
		It("should run one replica without leader election by default and at explicit replicas=1", func() {
			By("Clearing any pre-existing core componentConfig for a clean baseline")
			Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.CoreController, nil, nil)).To(Succeed())
			Eventually(func(g Gomega) {
				Expect(isExternalSecretsConfigDegraded(ctx)).To(BeFalse())
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Verifying default: replicas=1 and no leader election arg")
			assertCoreControllerReplicasAndLeaderElection(1, false)

			By("Setting explicit replicas=1")
			Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.CoreController,
				&operatorv1alpha1.DeploymentConfig{Replicas: ptr.To(int32(1))}, nil)).To(Succeed())
			assertCoreControllerReplicasAndLeaderElection(1, false)
		})

		It("should scale to 2 replicas with leader election and a held lease", func() {
			By("Setting core controller replicas=2")
			Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.CoreController,
				&operatorv1alpha1.DeploymentConfig{Replicas: ptr.To(int32(2))}, nil)).To(Succeed())

			By("Verifying Deployment scaled to 2/2 with the leader election arg")
			assertCoreControllerReplicasAndLeaderElection(2, true)

			By("Verifying a leader election lease is held by a core controller pod")
			Eventually(func(g Gomega) {
				pods, err := coreControllerPodNames(ctx, clientset)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(pods)).To(BeNumerically(">=", 2))
				foundLease := false
				for _, pod := range pods {
					lease, err := findLeaderLeaseForPod(ctx, clientset, pod)
					g.Expect(err).NotTo(HaveOccurred())
					if lease != nil {
						foundLease = true
						break
					}
				}
				g.Expect(foundLease).To(BeTrue(), "expected a leader election Lease held by a core controller pod in %s", operandNamespace)
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			Expect(utils.WaitForExternalSecretsConfigReady(ctx, dynamicClient, common.ExternalSecretsConfigObjectName, 2*time.Minute)).To(Succeed())
		})

		It("should fail over to a new leader when the leader pod is deleted", func() {
			By("Identifying the leader pod from the lease holder identity")
			var leaderPod string
			Eventually(func(g Gomega) {
				pods, err := coreControllerPodNames(ctx, clientset)
				g.Expect(err).NotTo(HaveOccurred())
				for _, pod := range pods {
					lease, err := findLeaderLeaseForPod(ctx, clientset, pod)
					g.Expect(err).NotTo(HaveOccurred())
					if lease != nil {
						leaderPod = pod
						return
					}
				}
				Fail("no leader election Lease held by a core controller pod found")
			}, time.Minute, 5*time.Second).Should(Succeed())

			By(fmt.Sprintf("Deleting the leader pod %s", leaderPod))
			Expect(clientset.CoreV1().Pods(operandNamespace).Delete(ctx, leaderPod, metav1.DeleteOptions{})).To(Succeed())

			By("Verifying a replacement pod becomes ready (2/2)")
			waitForDeploymentReplicas(ctx, clientset, externalsecrets.OperandCoreControllerDeployment, 2)

			By("Verifying the lease is now held by a different pod")
			Eventually(func(g Gomega) {
				pods, err := coreControllerPodNames(ctx, clientset)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pods).NotTo(ContainElement(leaderPod), "leader pod should have been replaced")
				newHolder := ""
				for _, pod := range pods {
					lease, err := findLeaderLeaseForPod(ctx, clientset, pod)
					g.Expect(err).NotTo(HaveOccurred())
					if lease != nil {
						newHolder = pod
						break
					}
				}
				g.Expect(newHolder).NotTo(BeEmpty(), "expected a new lease holder among core controller pods")
				g.Expect(newHolder).NotTo(Equal(leaderPod), "lease should have failed over to a different pod")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("Verifying ExternalSecretsConfig is Ready and not Degraded")
			Eventually(func(g Gomega) {
				g.Expect(isExternalSecretsConfigDegraded(ctx)).To(BeFalse())
			}, time.Minute, 5*time.Second).Should(Succeed())
			Expect(utils.WaitForExternalSecretsConfigReady(ctx, dynamicClient, common.ExternalSecretsConfigObjectName, 2*time.Minute)).To(Succeed())
		})

		It("should remove the leader election flag when scaled back to 1 replica", func() {
			By("Setting core controller replicas=1")
			Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.CoreController,
				&operatorv1alpha1.DeploymentConfig{Replicas: ptr.To(int32(1))}, nil)).To(Succeed())
			assertCoreControllerReplicasAndLeaderElection(1, false)
			Expect(utils.WaitForExternalSecretsConfigReady(ctx, dynamicClient, common.ExternalSecretsConfigObjectName, 2*time.Minute)).To(Succeed())
		})
	})

	Context("Webhook HA and per-component override isolation", Ordered, Label("Feature:Replicas", "Feature:AdvancedOverrides"), func() {
		It("should scale the webhook independently and keep overrides isolated from the core controller", func() {
			By("Fingerprinting the core controller pod template before any webhook change")
			coreDep, err := getDeployment(ctx, clientset, externalsecrets.OperandCoreControllerDeployment)
			Expect(err).NotTo(HaveOccurred())
			coreTemplateJSON, err := json.Marshal(coreDep.Spec.Template)
			Expect(err).NotTo(HaveOccurred())

			By("Setting webhook replicas=2")
			Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.Webhook,
				&operatorv1alpha1.DeploymentConfig{Replicas: ptr.To(int32(2))}, nil)).To(Succeed())
			waitForDeploymentReplicas(ctx, clientset, externalsecrets.OperandWebhookDeployment, 2)

			By("Verifying the webhook Service has 2 ready endpoints")
			Eventually(func(g Gomega) {
				endpoints, err := clientset.CoreV1().Endpoints(operandNamespace).Get(ctx, webhookServiceName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				readyAddresses := 0
				for _, subset := range endpoints.Subsets {
					readyAddresses += len(subset.Addresses)
				}
				g.Expect(readyAddresses).To(Equal(2), "webhook Service should list 2 ready endpoints")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("Verifying the webhook container has no leader election arg")
			Eventually(func(g Gomega) {
				dep, err := getDeployment(ctx, clientset, externalsecrets.OperandWebhookDeployment)
				g.Expect(err).NotTo(HaveOccurred())
				hasArg, found := deploymentContainerHasArg(dep, externalsecrets.OperandWebhookContainer, externalsecrets.LeaderElectionArg)
				g.Expect(found).To(BeTrue())
				g.Expect(hasArg).To(BeFalse(), "webhook container must not have the leader election arg")
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Applying advancedOverrides to the webhook only")
			webhookPodLabels, err := firstPodLabels(ctx, clientset, operandWebhookPodPrefix)
			Expect(err).NotTo(HaveOccurred())
			partOfValue := webhookPodLabels[partOfLabel]
			Expect(partOfValue).NotTo(BeEmpty(), "webhook pod should carry the %s label", partOfLabel)
			webhookOverride := affinityOnlyOverride(partOfLabel, partOfValue)
			Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.Webhook,
				&operatorv1alpha1.DeploymentConfig{Replicas: ptr.To(int32(2))}, mustRawExtension(webhookOverride))).To(Succeed())

			By("Verifying the webhook Deployment received the affinity")
			Eventually(func(g Gomega) {
				dep, err := getDeployment(ctx, clientset, externalsecrets.OperandWebhookDeployment)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(dep.Spec.Template.Spec.Affinity).NotTo(BeNil())
				g.Expect(dep.Spec.Template.Spec.Affinity.PodAntiAffinity).NotTo(BeNil())
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("Verifying the core controller pod template is unchanged")
			Consistently(func(g Gomega) {
				dep, err := getDeployment(ctx, clientset, externalsecrets.OperandCoreControllerDeployment)
				g.Expect(err).NotTo(HaveOccurred())
				current, err := json.Marshal(dep.Spec.Template)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(current).To(Equal(coreTemplateJSON), "core controller pod template must not be affected by webhook overrides")
			}, 30*time.Second, 5*time.Second).Should(Succeed())

			Expect(utils.WaitForExternalSecretsConfigReady(ctx, dynamicClient, common.ExternalSecretsConfigObjectName, 2*time.Minute)).To(Succeed())
		})
	})

	Context("Advanced overrides on the core controller", Ordered, Label("Feature:AdvancedOverrides"), func() {
		It("should apply all allowlisted scheduling fields", func() {
			By("Picking a ready node hostname and a label carried by core controller pods")
			hostname, err := firstReadyNodeHostname(ctx, clientset)
			Expect(err).NotTo(HaveOccurred())
			corePodLabels, err := firstPodLabels(ctx, clientset, operandCoreControllerPodPrefix)
			Expect(err).NotTo(HaveOccurred())
			partOfValue := corePodLabels[partOfLabel]
			Expect(partOfValue).NotTo(BeEmpty(), "core pod should carry the %s label", partOfLabel)

			By("Applying affinity + tolerations + topologySpreadConstraints + nodeSelector")
			Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.CoreController, nil,
				mustRawExtension(schedulingOverride(partOfLabel, partOfValue, hostname)))).To(Succeed())

			By("Verifying all four fields on the Deployment pod template")
			Eventually(func(g Gomega) {
				dep, err := getDeployment(ctx, clientset, externalsecrets.OperandCoreControllerDeployment)
				g.Expect(err).NotTo(HaveOccurred())
				ps := dep.Spec.Template.Spec
				g.Expect(ps.Affinity).NotTo(BeNil())
				g.Expect(ps.Affinity.PodAntiAffinity).NotTo(BeNil())
				g.Expect(len(ps.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution)).To(Equal(1))
				g.Expect(len(ps.Tolerations)).To(BeNumerically(">=", 1))
				g.Expect(ps.TopologySpreadConstraints).NotTo(BeEmpty())
				g.Expect(ps.TopologySpreadConstraints[0].WhenUnsatisfiable).To(Equal(corev1.ScheduleAnyway))
				g.Expect(ps.NodeSelector).To(HaveKeyWithValue("kubernetes.io/hostname", hostname))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("Verifying the running pod carries the nodeSelector and toleration")
			waitForDeploymentReplicas(ctx, clientset, externalsecrets.OperandCoreControllerDeployment, 1)
			Eventually(func(g Gomega) {
				pods, err := clientset.CoreV1().Pods(operandNamespace).List(ctx, metav1.ListOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				var running *corev1.Pod
				for i := range pods.Items {
					pod := &pods.Items[i]
					if pod.DeletionTimestamp == nil && strings.HasPrefix(pod.Name, operandCoreControllerPodPrefix) && pod.Status.Phase == corev1.PodRunning {
						running = pod
						break
					}
				}
				g.Expect(running).NotTo(BeNil(), "expected a running core controller pod")
				g.Expect(running.Spec.NodeSelector).To(HaveKeyWithValue("kubernetes.io/hostname", hostname))
				g.Expect(running.Spec.Tolerations).NotTo(BeEmpty())
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			Expect(utils.WaitForExternalSecretsConfigReady(ctx, dynamicClient, common.ExternalSecretsConfigObjectName, 2*time.Minute)).To(Succeed())
		})

		It("should apply container args and resources overrides and re-assert leader election", func() {
			const (
				overrideConcurrent = "--concurrent=20"
				overrideBurst      = "--client-burst=200"
				overrideQPS        = "--client-qps=100"
			)

			By("Setting core replicas=2 with args + resources advancedOverrides in one payload")
			Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.CoreController,
				&operatorv1alpha1.DeploymentConfig{Replicas: ptr.To(int32(2))},
				mustRawExtension(argsAndResourcesOverride(overrideConcurrent, overrideBurst, overrideQPS)))).To(Succeed())
			waitForDeploymentReplicas(ctx, clientset, externalsecrets.OperandCoreControllerDeployment, 2)

			By("Verifying custom args, re-asserted leader election arg, and exact resources on the Deployment")
			Eventually(func(g Gomega) {
				dep, err := getDeployment(ctx, clientset, externalsecrets.OperandCoreControllerDeployment)
				g.Expect(err).NotTo(HaveOccurred())
				args, found := getDeploymentContainerArgs(dep, externalsecrets.OperandCoreControllerContainer)
				g.Expect(found).To(BeTrue())
				for _, want := range []string{overrideConcurrent, overrideBurst, overrideQPS, externalsecrets.LeaderElectionArg} {
					g.Expect(args).To(ContainElement(want), "core controller args should include %q", want)
				}
				var container *corev1.Container
				for i := range dep.Spec.Template.Spec.Containers {
					if dep.Spec.Template.Spec.Containers[i].Name == externalsecrets.OperandCoreControllerContainer {
						container = &dep.Spec.Template.Spec.Containers[i]
					}
				}
				g.Expect(container).NotTo(BeNil())
				g.Expect(container.Resources.Requests.Cpu().String()).To(Equal("200m"))
				g.Expect(container.Resources.Requests.Memory().String()).To(Equal("256Mi"))
				g.Expect(container.Resources.Limits.Cpu().String()).To(Equal("500m"))
				g.Expect(container.Resources.Limits.Memory().String()).To(Equal("512Mi"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("Verifying a running pod starts with the overridden concurrent flag")
			Eventually(func(g Gomega) {
				pods, err := clientset.CoreV1().Pods(operandNamespace).List(ctx, metav1.ListOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				var running *corev1.Pod
				for i := range pods.Items {
					pod := &pods.Items[i]
					if pod.DeletionTimestamp == nil && strings.HasPrefix(pod.Name, operandCoreControllerPodPrefix) && pod.Status.Phase == corev1.PodRunning {
						running = pod
						break
					}
				}
				g.Expect(running).NotTo(BeNil(), "expected a running core controller pod")
				var container *corev1.Container
				for i := range running.Spec.Containers {
					if running.Spec.Containers[i].Name == externalsecrets.OperandCoreControllerContainer {
						container = &running.Spec.Containers[i]
					}
				}
				g.Expect(container).NotTo(BeNil())
				g.Expect(container.Args).To(ContainElement(overrideConcurrent))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			Expect(utils.WaitForExternalSecretsConfigReady(ctx, dynamicClient, common.ExternalSecretsConfigObjectName, 2*time.Minute)).To(Succeed())
		})
	})

	Context("Conditional components", Ordered, Label("Feature:Replicas", "Feature:AdvancedOverrides"), func() {
		It("should apply replicas to cert-controller or bitwarden-sdk-server when present", func() {
			esc := &operatorv1alpha1.ExternalSecretsConfig{}
			Expect(runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, esc)).To(Succeed())

			certExpected := utils.IsCertControllerExpected(esc)
			bitwardenEnabled := esc.Spec.Plugins.BitwardenSecretManagerProvider != nil &&
				esc.Spec.Plugins.BitwardenSecretManagerProvider.Mode == operatorv1alpha1.Enabled
			if !certExpected && !bitwardenEnabled {
				Skip("neither cert-controller (cert-manager enabled) nor bitwarden plugin (disabled) is present in this cluster")
			}

			if certExpected {
				By("Scaling cert-controller to 2 replicas")
				Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.CertController,
					&operatorv1alpha1.DeploymentConfig{Replicas: ptr.To(int32(2))}, nil)).To(Succeed())
				waitForDeploymentReplicas(ctx, clientset, externalsecrets.OperandCertControllerDeployment, 2)
				Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.CertController, nil, nil)).To(Succeed())
				waitForDeploymentReplicas(ctx, clientset, externalsecrets.OperandCertControllerDeployment, 1)
			}

			if bitwardenEnabled {
				By("Scaling bitwarden-sdk-server to 2 replicas")
				Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.BitwardenSDKServer,
					&operatorv1alpha1.DeploymentConfig{Replicas: ptr.To(int32(2))}, nil)).To(Succeed())
				waitForDeploymentReplicas(ctx, clientset, externalsecrets.OperandBitwardenSDKServerDeployment, 2)
				Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.BitwardenSDKServer, nil, nil)).To(Succeed())
				waitForDeploymentReplicas(ctx, clientset, externalsecrets.OperandBitwardenSDKServerDeployment, 1)
			}

			Expect(utils.WaitForExternalSecretsConfigReady(ctx, dynamicClient, common.ExternalSecretsConfigObjectName, 2*time.Minute)).To(Succeed())
		})
	})

	Context("Disallowed advancedOverrides paths", Ordered, Label("Feature:AdvancedOverrides"), func() {
		It("should reject disallowed pod-spec paths with Degraded and no partial apply", func() {
			payloads := []struct {
				name        string
				expectedMsg string
				override    any
			}{
				{
					name:        "volumes",
					expectedMsg: "volumes",
					override:    disallowedPodSpecOverride(map[string]any{"volumes": []any{map[string]any{"name": "e2e-evil-volume", "emptyDir": map[string]any{}}}}),
				},
				{
					name:        "initContainers",
					expectedMsg: "initContainers",
					override:    disallowedPodSpecOverride(map[string]any{"initContainers": []any{map[string]any{"name": "e2e-evil-init", "image": "busybox"}}}),
				},
				{
					name:        "spec.template.metadata",
					expectedMsg: "spec.template.metadata",
					override:    map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"e2e-evil": "label"}}}}},
				},
				{
					name:        "serviceAccountName",
					expectedMsg: "serviceAccountName",
					override:    disallowedPodSpecOverride(map[string]any{"serviceAccountName": "e2e-evil-sa"}),
				},
			}

			for _, tc := range payloads {
				tc := tc
				It(fmt.Sprintf("should Degraded for disallowed path %s and leave the Deployment unchanged", tc.name), func() {
					By("Recording the core controller Deployment spec before the invalid override")
					beforeJSON := coreDeploymentSpecJSON(ctx)

					By("Applying the disallowed advancedOverrides payload")
					Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.CoreController, nil, mustRawExtension(tc.override))).To(Succeed())

					By("Waiting for ExternalSecretsConfig to become Degraded with the disallowed path named")
					Eventually(func(g Gomega) {
						g.Expect(isExternalSecretsConfigDegraded(ctx)).To(BeTrue(), "ExternalSecretsConfig should be Degraded for disallowed path %s", tc.name)
						msg := externalSecretsConfigDegradedMessage(ctx)
						g.Expect(msg).To(ContainSubstring(tc.expectedMsg))
					}, 3*time.Minute, 5*time.Second).Should(Succeed())

					By("Verifying the core controller Deployment spec is unchanged (no partial apply)")
					Consistently(func(g Gomega) {
						g.Expect(coreDeploymentSpecJSON(ctx)).To(Equal(beforeJSON), "Deployment spec must not change for disallowed path %s", tc.name)
					}, 30*time.Second, 5*time.Second).Should(Succeed())

					By("Clearing the override and waiting for recovery")
					Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.CoreController, nil, nil)).To(Succeed())
					Eventually(func(g Gomega) {
						g.Expect(isExternalSecretsConfigDegraded(ctx)).To(BeFalse())
					}, time.Minute, 5*time.Second).Should(Succeed())
					Expect(utils.WaitForExternalSecretsConfigReady(ctx, dynamicClient, common.ExternalSecretsConfigObjectName, 2*time.Minute)).To(Succeed())
				})
			}
		})

		It("should reject disallowed container fields and spec.replicas with Degraded", func() {
			payloads := []struct {
				name        string
				expectedMsg string
				override    any
				replicas    *int32
			}{
				{
					name:        "container image",
					expectedMsg: `"image"`,
					override:    disallowedContainerFieldOverride(map[string]any{"image": "e2e-evil-image:latest"}),
				},
				{
					name:        "container env",
					expectedMsg: `"env"`,
					override:    disallowedContainerFieldOverride(map[string]any{"env": []any{map[string]any{"name": "E2E_BAD", "value": "bad"}}}),
				},
				{
					name:        "container ports",
					expectedMsg: `"ports"`,
					override:    disallowedContainerFieldOverride(map[string]any{"ports": []any{map[string]any{"containerPort": 9999}}}),
				},
				{
					name:        "spec.replicas",
					expectedMsg: "spec.replicas",
					override:    map[string]any{"spec": map[string]any{"replicas": 3}},
					replicas:    ptr.To(int32(1)),
				},
			}

			for _, tc := range payloads {
				tc := tc
				It(fmt.Sprintf("should Degraded for disallowed %s and leave the Deployment unchanged", tc.name), func() {
					By("Recording the core controller Deployment spec before the invalid override")
					beforeJSON := coreDeploymentSpecJSON(ctx)

					By("Applying the disallowed advancedOverrides payload")
					Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.CoreController, nil, mustRawExtension(tc.override))).To(Succeed())

					By("Waiting for ExternalSecretsConfig to become Degraded with the disallowed field named")
					Eventually(func(g Gomega) {
						g.Expect(isExternalSecretsConfigDegraded(ctx)).To(BeTrue(), "ExternalSecretsConfig should be Degraded for disallowed %s", tc.name)
						msg := externalSecretsConfigDegradedMessage(ctx)
						g.Expect(msg).To(ContainSubstring(tc.expectedMsg))
					}, 3*time.Minute, 5*time.Second).Should(Succeed())

					By("Verifying the core controller Deployment spec is unchanged")
					Consistently(func(g Gomega) {
						g.Expect(coreDeploymentSpecJSON(ctx)).To(Equal(beforeJSON), "Deployment spec must not change for disallowed %s", tc.name)
					}, 30*time.Second, 5*time.Second).Should(Succeed())

					By("Clearing the override and waiting for recovery")
					Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.CoreController, nil, nil)).To(Succeed())
					Eventually(func(g Gomega) {
						g.Expect(isExternalSecretsConfigDegraded(ctx)).To(BeFalse())
					}, time.Minute, 5*time.Second).Should(Succeed())
					Expect(utils.WaitForExternalSecretsConfigReady(ctx, dynamicClient, common.ExternalSecretsConfigObjectName, 2*time.Minute)).To(Succeed())
				})
			}
		})

		It("should recover from Degraded when the invalid override is corrected to an allowlisted payload", func() {
			By("Applying a disallowed payload (volumes)")
			Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.CoreController, nil,
				mustRawExtension(disallowedPodSpecOverride(map[string]any{"volumes": []any{map[string]any{"name": "e2e-evil-volume", "emptyDir": map[string]any{}}}})))).To(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(isExternalSecretsConfigDegraded(ctx)).To(BeTrue())
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("Correcting the override to an allowlisted nodeSelector payload")
			hostname, err := firstReadyNodeHostname(ctx, clientset)
			Expect(err).NotTo(HaveOccurred())
			Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.CoreController, nil,
				mustRawExtension(schedulingOverride(partOfLabelFromCorePod(ctx, clientset), partOfLabelFromCorePod(ctx, clientset), hostname)))).To(Succeed())

			By("Waiting for ExternalSecretsConfig to leave Degraded and become Ready")
			Eventually(func(g Gomega) {
				g.Expect(isExternalSecretsConfigDegraded(ctx)).To(BeFalse(), "ExternalSecretsConfig should leave Degraded after the fix")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
			Expect(utils.WaitForExternalSecretsConfigReady(ctx, dynamicClient, common.ExternalSecretsConfigObjectName, 2*time.Minute)).To(Succeed())

			By("Verifying the allowlisted nodeSelector was applied and the pod is Ready")
			Eventually(func(g Gomega) {
				dep, err := getDeployment(ctx, clientset, externalsecrets.OperandCoreControllerDeployment)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(dep.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("kubernetes.io/hostname", hostname))
				g.Expect(dep.Status.ReadyReplicas).To(BeNumerically("==", 1))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("Clearing the override")
			Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.CoreController, nil, nil)).To(Succeed())
			Eventually(func(g Gomega) {
				dep, err := getDeployment(ctx, clientset, externalsecrets.OperandCoreControllerDeployment)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(dep.Spec.Template.Spec.NodeSelector).To(BeEmpty())
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
			Expect(utils.WaitForExternalSecretsConfigReady(ctx, dynamicClient, common.ExternalSecretsConfigObjectName, 2*time.Minute)).To(Succeed())
		})
	})

	Context("Configuration persistence across operator restart", Ordered, Label("Feature:Replicas", "Feature:AdvancedOverrides"), func() {
		It("should keep replicas and args overrides after the operator pod restarts", func() {
			const overrideConcurrent = "--concurrent=20"

			By("Applying core replicas=2 with an args override")
			Expect(updateComponentConfig(ctx, runtimeClient, operatorv1alpha1.CoreController,
				&operatorv1alpha1.DeploymentConfig{Replicas: ptr.To(int32(2))},
				mustRawExtension(argsOverride(overrideConcurrent)))).To(Succeed())
			waitForDeploymentReplicas(ctx, clientset, externalsecrets.OperandCoreControllerDeployment, 2)
			Eventually(func(g Gomega) {
				dep, err := getDeployment(ctx, clientset, externalsecrets.OperandCoreControllerDeployment)
				g.Expect(err).NotTo(HaveOccurred())
				args, found := getDeploymentContainerArgs(dep, externalsecrets.OperandCoreControllerContainer)
				g.Expect(found).To(BeTrue())
				g.Expect(args).To(ContainElement(overrideConcurrent))
				g.Expect(args).To(ContainElement(externalsecrets.LeaderElectionArg))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("Deleting the operator pod(s)")
			pods, err := clientset.CoreV1().Pods(operatorNamespace).List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			for i := range pods.Items {
				pod := &pods.Items[i]
				if pod.DeletionTimestamp == nil && strings.HasPrefix(pod.Name, operatorPodPrefix) {
					Expect(clientset.CoreV1().Pods(operatorNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})).To(Succeed())
				}
			}

			By("Waiting for the operator pod to be Ready again")
			Expect(utils.VerifyPodsReadyByPrefix(ctx, clientset, operatorNamespace, []string{operatorPodPrefix})).To(Succeed())

			By("Verifying replicas, args override, and leader election arg persisted")
			Consistently(func(g Gomega) {
				dep, err := getDeployment(ctx, clientset, externalsecrets.OperandCoreControllerDeployment)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(dep.Spec.Replicas).NotTo(BeNil())
				g.Expect(*dep.Spec.Replicas).To(Equal(int32(2)), "replicas must survive operator restart")
				args, found := getDeploymentContainerArgs(dep, externalsecrets.OperandCoreControllerContainer)
				g.Expect(found).To(BeTrue())
				g.Expect(args).To(ContainElement(overrideConcurrent), "args override must survive operator restart")
				g.Expect(args).To(ContainElement(externalsecrets.LeaderElectionArg), "leader election arg must survive operator restart")
			}, 30*time.Second, 5*time.Second).Should(Succeed())

			Expect(utils.WaitForExternalSecretsConfigReady(ctx, dynamicClient, common.ExternalSecretsConfigObjectName, 2*time.Minute)).To(Succeed())
			waitForDeploymentReplicas(ctx, clientset, externalsecrets.OperandCoreControllerDeployment, 2)
		})
	})
})

// updateComponentConfig replaces the componentConfigs entry for the given component with the
// supplied deploymentConfigs/advancedOverrides, or removes the entry when both are nil.
func updateComponentConfig(ctx context.Context, c client.Client, name operatorv1alpha1.ComponentName, dc *operatorv1alpha1.DeploymentConfig, ao *runtime.RawExtension) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		esc := &operatorv1alpha1.ExternalSecretsConfig{}
		if err := c.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, esc); err != nil {
			return err
		}
		configs := make([]operatorv1alpha1.ComponentConfig, 0, len(esc.Spec.ControllerConfig.ComponentConfigs)+1)
		found := false
		for _, cc := range esc.Spec.ControllerConfig.ComponentConfigs {
			if cc.ComponentName == name {
				found = true
				if dc != nil || ao != nil {
					configs = append(configs, operatorv1alpha1.ComponentConfig{ComponentName: name, DeploymentConfigs: dc, AdvancedOverrides: ao})
				}
				continue
			}
			configs = append(configs, cc)
		}
		if !found && (dc != nil || ao != nil) {
			configs = append(configs, operatorv1alpha1.ComponentConfig{ComponentName: name, DeploymentConfigs: dc, AdvancedOverrides: ao})
		}
		esc.Spec.ControllerConfig.ComponentConfigs = configs
		return c.Update(ctx, esc)
	})
}

func getDeployment(ctx context.Context, clientset *kubernetes.Clientset, name string) (*appsv1.Deployment, error) {
	return clientset.AppsV1().Deployments(operandNamespace).Get(ctx, name, metav1.GetOptions{})
}

func waitForDeploymentReplicas(ctx context.Context, clientset *kubernetes.Clientset, name string, want int32) {
	Eventually(func(g Gomega) {
		dep, err := getDeployment(ctx, clientset, name)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(dep.Spec.Replicas).NotTo(BeNil())
		g.Expect(*dep.Spec.Replicas).To(Equal(want), "%s spec.replicas should be %d", name, want)
		g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", want), "%s should have %d ready replicas", name, want)
	}, 3*time.Minute, 5*time.Second).Should(Succeed())
}

func assertCoreControllerReplicasAndLeaderElection(wantReplicas int32, wantLeaderElection bool) {
	Eventually(func(g Gomega) {
		dep, err := getDeployment(context.Background(), suiteClientset, externalsecrets.OperandCoreControllerDeployment)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(dep.Spec.Replicas).NotTo(BeNil())
		g.Expect(*dep.Spec.Replicas).To(Equal(wantReplicas))
		g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", wantReplicas))
		hasArg, found := deploymentContainerHasArg(dep, externalsecrets.OperandCoreControllerContainer, externalsecrets.LeaderElectionArg)
		g.Expect(found).To(BeTrue(), "core controller container should exist")
		g.Expect(hasArg).To(Equal(wantLeaderElection), "leader election arg presence should be %v at replicas=%d", wantLeaderElection, wantReplicas)
	}, 3*time.Minute, 5*time.Second).Should(Succeed())
}

func coreControllerPodNames(ctx context.Context, clientset *kubernetes.Clientset) ([]string, error) {
	pods, err := clientset.CoreV1().Pods(operandNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var names []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil || !strings.HasPrefix(pod.Name, operandCoreControllerPodPrefix) {
			continue
		}
		names = append(names, pod.Name)
	}
	return names, nil
}

// findLeaderLeaseForPod returns the Lease in the operand namespace whose holder identity
// references the given pod (controller-runtime sets the holder to the pod hostname), or nil.
func findLeaderLeaseForPod(ctx context.Context, clientset *kubernetes.Clientset, podName string) (*coordinationv1.Lease, error) {
	leases, err := clientset.CoordinationV1().Leases(operandNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range leases.Items {
		lease := &leases.Items[i]
		if lease.Spec.HolderIdentity == nil {
			continue
		}
		if *lease.Spec.HolderIdentity == podName || strings.HasPrefix(*lease.Spec.HolderIdentity, podName) {
			return lease.DeepCopy(), nil
		}
	}
	return nil, nil
}

func firstPodLabels(ctx context.Context, clientset *kubernetes.Clientset, podPrefix string) (map[string]string, error) {
	pods, err := clientset.CoreV1().Pods(operandNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp == nil && strings.HasPrefix(pod.Name, podPrefix) {
			return pod.Labels, nil
		}
	}
	return nil, fmt.Errorf("no running pod found with prefix %s in %s", podPrefix, operandNamespace)
}

func firstReadyNodeHostname(ctx context.Context, clientset *kubernetes.Clientset) (string, error) {
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		ready := false
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if !ready {
			continue
		}
		if hostname := node.Labels["kubernetes.io/hostname"]; hostname != "" {
			return hostname, nil
		}
		return node.Name, nil
	}
	return "", fmt.Errorf("no Ready node found in the cluster")
}

func partOfLabelFromCorePod(ctx context.Context, clientset *kubernetes.Clientset) string {
	labels, err := firstPodLabels(ctx, clientset, operandCoreControllerPodPrefix)
	if err != nil {
		return ""
	}
	return labels[partOfLabel]
}

func mustRawExtension(v any) *runtime.RawExtension {
	data, err := json.Marshal(v)
	Expect(err).NotTo(HaveOccurred())
	return &runtime.RawExtension{Raw: data}
}

func coreDeploymentSpecJSON(ctx context.Context) []byte {
	dep, err := getDeployment(ctx, suiteClientset, externalsecrets.OperandCoreControllerDeployment)
	Expect(err).NotTo(HaveOccurred())
	data, err := json.Marshal(dep.Spec)
	Expect(err).NotTo(HaveOccurred())
	return data
}

// affinityOnlyOverride builds an advancedOverrides payload with only a preferred
// podAntiAffinity using the given label selector.
func affinityOnlyOverride(labelKey, labelValue string) map[string]any {
	return map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"affinity": map[string]any{
						"podAntiAffinity": map[string]any{
							"preferredDuringSchedulingIgnoredDuringExecution": []any{
								map[string]any{
									"weight": 100,
									"podAffinityTerm": map[string]any{
										"labelSelector": map[string]any{
											"matchLabels": map[string]any{labelKey: labelValue},
										},
										"topologyKey": "kubernetes.io/hostname",
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// schedulingOverride builds an advancedOverrides payload with all four allowlisted
// pod-spec scheduling fields.
func schedulingOverride(labelKey, labelValue, hostname string) map[string]any {
	return map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"affinity": map[string]any{
						"podAntiAffinity": map[string]any{
							"preferredDuringSchedulingIgnoredDuringExecution": []any{
								map[string]any{
									"weight": 100,
									"podAffinityTerm": map[string]any{
										"labelSelector": map[string]any{
											"matchLabels": map[string]any{labelKey: labelValue},
										},
										"topologyKey": "kubernetes.io/hostname",
									},
								},
							},
						},
					},
					"tolerations": []any{
						map[string]any{
							"key":      "e2e-advanced-overrides",
							"operator": "Exists",
							"effect":   "NoSchedule",
						},
					},
					"topologySpreadConstraints": []any{
						map[string]any{
							"maxSkew":           1,
							"topologyKey":       "kubernetes.io/hostname",
							"whenUnsatisfiable": "ScheduleAnyway",
							"labelSelector": map[string]any{
								"matchLabels": map[string]any{labelKey: labelValue},
							},
						},
					},
					"nodeSelector": map[string]any{
						"kubernetes.io/hostname": hostname,
					},
				},
			},
		},
	}
}

// argsOverride builds an advancedOverrides payload replacing the core controller
// container args with the given flags.
func argsOverride(args ...string) map[string]any {
	return containerFieldOverride(map[string]any{"args": args})
}

// argsAndResourcesOverride builds an advancedOverrides payload replacing the core
// controller container args and setting fixed resource requests/limits.
func argsAndResourcesOverride(args ...string) map[string]any {
	return containerFieldOverride(map[string]any{
		"args": args,
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "200m", "memory": "256Mi"},
			"limits":   map[string]any{"cpu": "500m", "memory": "512Mi"},
		},
	})
}

func containerFieldOverride(fields map[string]any) map[string]any {
	entry := map[string]any{"name": externalsecrets.OperandCoreControllerContainer}
	for k, v := range fields {
		entry[k] = v
	}
	return map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{entry},
				},
			},
		},
	}
}

// disallowedPodSpecOverride wraps a single pod-spec field under spec.template.spec.
func disallowedPodSpecOverride(field map[string]any) map[string]any {
	spec := map[string]any{}
	for k, v := range field {
		spec[k] = v
	}
	return map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": spec,
			},
		},
	}
}

// disallowedContainerFieldOverride wraps a single disallowed container field on the
// core controller container entry.
func disallowedContainerFieldOverride(field map[string]any) map[string]any {
	entry := map[string]any{"name": externalsecrets.OperandCoreControllerContainer}
	for k, v := range field {
		entry[k] = v
	}
	return containerFieldOverride(entry)
}
