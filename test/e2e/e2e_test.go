//go:build e2e
// +build e2e

/*
Copyright 2025.
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
	"embed"
	"encoding/base64"
	"fmt"
	"maps"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	intstr "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"

	operatorv1alpha1 "github.com/openshift/external-secrets-operator/api/v1alpha1"
	"github.com/openshift/external-secrets-operator/pkg/controller/common"
	externalsecrets "github.com/openshift/external-secrets-operator/pkg/controller/external_secrets"
	"github.com/openshift/external-secrets-operator/test/utils"
)

//go:embed testdata/*
var testassets embed.FS

const (
	// test bindata
	externalSecretsFile                  = "testdata/external_secret.yaml"
	externalSecretsFileWithRevisionLimit = "testdata/external_secret_with_revision_limits.yaml"
	expectedSecretValueFile              = "testdata/expected_value.yaml"
)

const (
	// test resource names
	operatorNamespace              = common.ExternalSecretsOperatorCommonName
	operandNamespace               = externalsecrets.OperandDefaultNamespace
	operatorPodPrefix              = common.ExternalSecretsOperatorCommonName + "-controller-manager-"
	operandCoreControllerPodPrefix = externalsecrets.OperandCoreControllerPodPrefix
	operandCertControllerPodPrefix = externalsecrets.OperandCertControllerPodPrefix
	operandWebhookPodPrefix        = externalsecrets.OperandWebhookPodPrefix
	testNamespacePrefix            = "external-secrets-e2e-test-"
)

const (
	externalSecretsGroupName = "external-secrets.io"
	v1APIVersion             = "v1"
	v1alpha1APIVersion       = "v1alpha1"
	clusterSecretStoresKind  = "clustersecretstores"
	PushSecretsKind          = "pushsecrets"
	externalSecretsKind      = "externalsecrets"
)

const (
	userNPPrefix            = "eso-user-"
	npProxyEgressPolicyName = "eso-sys-allow-proxy-egress"
	managedByLabel          = "app.kubernetes.io/managed-by"
	partOfLabel             = "app.kubernetes.io/part-of"
	managedByValue          = "external-secrets-operator"
)

var expectedStaticNPNames = []string{
	"eso-sys-deny-all-traffic",
	"eso-sys-allow-api-server-egress-for-main-controller",
	"eso-sys-allow-api-server-egress-for-webhook",
	"eso-sys-allow-to-dns",
}

var _ = Describe("External Secrets Operator End-to-End test scenarios", Ordered, func() {
	ctx := context.Background()
	var (
		clientset     *kubernetes.Clientset
		dynamicClient *dynamic.DynamicClient
		runtimeClient client.Client
		loader        utils.DynamicResourceLoader
		awsSecretName string
		testNamespace string
	)

	BeforeAll(func() {
		var err error
		loader = utils.NewDynamicResourceLoader(ctx, &testing.T{})

		clientset = suiteClientset
		dynamicClient = suiteDynamicClient
		runtimeClient = suiteRuntimeClient

		awsSecretName = fmt.Sprintf("eso-e2e-secret-%s", utils.GetRandomString(5))

		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"e2e-test": "true",
					"operator": "openshift-external-secrets-operator",
				},
				GenerateName: testNamespacePrefix,
			},
		}
		By("Creating the test namespace")
		got, err := clientset.CoreV1().Namespaces().Create(context.Background(), namespace, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "failed to create test namespace")
		testNamespace = got.GetName()

		By("Waiting for operator pod to be ready")
		Expect(utils.VerifyPodsReadyByPrefix(ctx, clientset, operatorNamespace, []string{
			operatorPodPrefix,
		})).To(Succeed())

		By("Ensuring ExternalSecretsConfig cluster CR exists and is Ready")
		Expect(ensureExternalSecretsConfigReady(ctx)).To(Succeed(),
			"ExternalSecretsConfig should have Ready=True and Degraded=False conditions")
	})

	BeforeEach(func() {
		By("Verifying external-secrets operand pods are ready")
		esc := &operatorv1alpha1.ExternalSecretsConfig{}
		Expect(runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, esc)).To(Succeed())
		Expect(utils.VerifyOperandPodsReady(ctx, clientset, operandNamespace, esc)).To(Succeed())
	})

	AfterEach(func() {
		if !CurrentSpecReport().State.Is(types.SpecStateFailureStates) {
			return
		}
		artifactDir := getTestDir()
		By(fmt.Sprintf("Test failed: dumping logs and resources to %s/e2e-artifacts/", artifactDir))
		if err := utils.DumpE2EArtifacts(ctx, clientset, dynamicClient, operatorNamespace, operandNamespace, testNamespace, artifactDir); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "warning: failed to dump e2e artifacts: %v\n", err)
		}
	})

	Context("AWS Secret Manager", Label("Platform:AWS", "Provider:AWS"), func() {
		const (
			clusterSecretStoreFile           = "testdata/aws_secret_store.yaml"
			externalSecretFile               = "testdata/aws_external_secret.yaml"
			pushSecretFile                   = "testdata/push_secret.yaml"
			awsSecretToPushFile              = "testdata/aws_k8s_push_secret.yaml"
			awsSecretNamePattern             = "${AWS_SECRET_KEY_NAME}"
			awsSecretValuePattern            = "${SECRET_VALUE}"
			awsClusterSecretStoreNamePattern = "${CLUSTERSECRETSTORE_NAME}"
			awsSecretRegionName              = "ap-south-1"
		)

		BeforeAll(func() {
			_, _, err := utils.FetchAWSCredsFromSecret(ctx, clientset, utils.AWSCredSecretName, utils.AWSCredNamespace)
			Expect(err).NotTo(HaveOccurred(),
				"AWS credentials secret %s/%s required (keys: aws_access_key_id, aws_secret_access_key). See test/e2e/README.md",
				utils.AWSCredNamespace, utils.AWSCredSecretName)
		})

		AfterAll(func() {
			By("Deleting the AWS secret")
			Expect(utils.DeleteAWSSecret(ctx, clientset, awsSecretName, awsSecretRegionName)).
				NotTo(HaveOccurred(), "failed to delete AWS secret test/e2e")
		})

		It("should create secrets mentioned in ExternalSecret using the referenced ClusterSecretStore", func() {
			var (
				clusterSecretStoreResourceName = fmt.Sprintf("aws-secret-store-%s", utils.GetRandomString(5))
				pushSecretResourceName         = "aws-push-secret"
				externalSecretResourceName     = "aws-external-secret"
				secretResourceName             = "aws-secret"
				keyNameInSecret                = "aws_secret_access_key"
			)

			defer func() {
				Expect(utils.DeleteAWSSecret(ctx, clientset, awsSecretName, awsSecretRegionName)).
					NotTo(HaveOccurred(), "failed to delete AWS secret test/e2e")
			}()

			expectedSecretValue, err := utils.ReadExpectedSecretValue(expectedSecretValueFile)
			Expect(err).To(Succeed())

			By("Creating kubernetes secret to be used in PushSecret")
			secretsAssetFunc := utils.ReplacePatternInAsset(awsSecretValuePattern, base64.StdEncoding.EncodeToString(expectedSecretValue))
			loader.CreateFromFile(secretsAssetFunc, awsSecretToPushFile, testNamespace)
			defer loader.DeleteFromFile(testassets.ReadFile, awsSecretToPushFile, testNamespace)

			By("Creating ClusterSecretStore")
			cssAssetFunc := utils.ReplacePatternInAsset(awsClusterSecretStoreNamePattern, clusterSecretStoreResourceName)
			loader.CreateFromFile(cssAssetFunc, clusterSecretStoreFile, testNamespace)
			defer loader.DeleteFromFile(cssAssetFunc, clusterSecretStoreFile, testNamespace)

			By("Waiting for ClusterSecretStore to become Ready")
			Expect(utils.WaitForESOResourceReady(ctx, dynamicClient,
				schema.GroupVersionResource{
					Group:    externalSecretsGroupName,
					Version:  v1APIVersion,
					Resource: clusterSecretStoresKind,
				},
				"", clusterSecretStoreResourceName, time.Minute,
			)).To(Succeed())

			By("Creating PushSecret")
			assetFunc := utils.ReplacePatternInAsset(awsSecretNamePattern, awsSecretName,
				awsClusterSecretStoreNamePattern, clusterSecretStoreResourceName)
			loader.CreateFromFile(assetFunc, pushSecretFile, testNamespace)
			defer loader.DeleteFromFile(testassets.ReadFile, pushSecretFile, testNamespace)

			By("Waiting for PushSecret to become Ready")
			Expect(utils.WaitForESOResourceReady(ctx, dynamicClient,
				schema.GroupVersionResource{
					Group:    externalSecretsGroupName,
					Version:  v1alpha1APIVersion,
					Resource: PushSecretsKind,
				},
				testNamespace, pushSecretResourceName, time.Minute,
			)).To(Succeed())

			By("Creating ExternalSecret")
			loader.CreateFromFile(assetFunc, externalSecretFile, testNamespace)
			defer loader.DeleteFromFile(testassets.ReadFile, externalSecretFile, testNamespace)

			By("Waiting for ExternalSecret to become Ready")
			Expect(utils.WaitForESOResourceReady(ctx, dynamicClient,
				schema.GroupVersionResource{
					Group:    externalSecretsGroupName,
					Version:  v1APIVersion,
					Resource: externalSecretsKind,
				},
				testNamespace, externalSecretResourceName, time.Minute,
			)).To(Succeed())

			By("Waiting for target secret to be created with expected data")
			Eventually(func(g Gomega) {
				secret, err := loader.KubeClient.CoreV1().Secrets(testNamespace).Get(ctx, secretResourceName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "should get %s from namespace %s", secretResourceName, testNamespace)

				val, ok := secret.Data[keyNameInSecret]
				g.Expect(ok).To(BeTrue(), "%s should be present in secret %s", keyNameInSecret, secret.Name)

				g.Expect(val).To(Equal(expectedSecretValue), "%s does not match expected value", keyNameInSecret)
			}, time.Minute, 10*time.Second).Should(Succeed())
		})
	})

	Context("Cross-platform: GCP cluster and AWS Secrets Manager", Label("Platform:GCP", "Provider:AWS"), func() {
		const (
			externalSecretFile               = "testdata/aws_external_secret.yaml"
			pushSecretFile                   = "testdata/push_secret.yaml"
			awsSecretToPushFile              = "testdata/aws_k8s_push_secret.yaml"
			awsSecretNamePattern             = "${AWS_SECRET_KEY_NAME}"
			awsSecretValuePattern            = "${SECRET_VALUE}"
			awsClusterSecretStoreNamePattern = "${CLUSTERSECRETSTORE_NAME}"
			awsSecretRegionName              = "ap-south-1"
		)
		var crossPlatformAWSSecretName string

		BeforeAll(func() {
			_, _, err := utils.FetchAWSCredsFromSecret(ctx, clientset, utils.AWSCredSecretName, utils.AWSCredNamespace)
			Expect(err).NotTo(HaveOccurred(),
				"AWS credentials secret %s/%s required (keys: aws_access_key_id, aws_secret_access_key). See test/e2e/README.md",
				utils.AWSCredNamespace, utils.AWSCredSecretName)
		})

		AfterAll(func() {
			if crossPlatformAWSSecretName != "" {
				By("Deleting the AWS secret")
				Expect(utils.DeleteAWSSecretFromCredsSecret(ctx, clientset, utils.AWSCredSecretName, utils.AWSCredNamespace, crossPlatformAWSSecretName, awsSecretRegionName)).
					NotTo(HaveOccurred(), "failed to delete AWS secret (cross-platform e2e)")
			}
		})

		It("should create secrets using ClusterSecretStore with AWS credentials secret in fixed namespace", func() {
			var (
				clusterSecretStoreResourceName = fmt.Sprintf("aws-secret-store-cross-%s", utils.GetRandomString(5))
				pushSecretResourceName         = "aws-push-secret"
				externalSecretResourceName     = "aws-external-secret"
				secretResourceName             = "aws-secret"
				keyNameInSecret                = "aws_secret_access_key"
			)

			crossPlatformAWSSecretName = fmt.Sprintf("e2e-cross-platform-%s", utils.GetRandomString(8))
			defer func() {
				if crossPlatformAWSSecretName != "" {
					_ = utils.DeleteAWSSecretFromCredsSecret(ctx, clientset, utils.AWSCredSecretName, utils.AWSCredNamespace, crossPlatformAWSSecretName, awsSecretRegionName)
				}
			}()

			expectedSecretValue, err := utils.ReadExpectedSecretValue(expectedSecretValueFile)
			Expect(err).To(Succeed())

			By("Creating kubernetes secret to be used in PushSecret")
			secretsAssetFunc := utils.ReplacePatternInAsset(awsSecretValuePattern, base64.StdEncoding.EncodeToString(expectedSecretValue))
			loader.CreateFromFile(secretsAssetFunc, awsSecretToPushFile, testNamespace)
			defer loader.DeleteFromFile(testassets.ReadFile, awsSecretToPushFile, testNamespace)

			By("Creating ClusterSecretStore (AWS) from API")
			cssObj := utils.AWSClusterSecretStore(clusterSecretStoreResourceName, awsSecretRegionName)
			loader.CreateFromUnstructured(cssObj, "")
			defer loader.DeleteFromUnstructured(cssObj, "")

			By("Waiting for ClusterSecretStore to become Ready")
			Expect(utils.WaitForESOResourceReady(ctx, dynamicClient,
				schema.GroupVersionResource{
					Group:    externalSecretsGroupName,
					Version:  v1APIVersion,
					Resource: clusterSecretStoresKind,
				},
				"", clusterSecretStoreResourceName, time.Minute,
			)).To(Succeed())

			By("Creating PushSecret")
			assetFunc := utils.ReplacePatternInAsset(awsSecretNamePattern, crossPlatformAWSSecretName,
				awsClusterSecretStoreNamePattern, clusterSecretStoreResourceName)
			loader.CreateFromFile(assetFunc, pushSecretFile, testNamespace)
			defer loader.DeleteFromFile(testassets.ReadFile, pushSecretFile, testNamespace)

			By("Waiting for PushSecret to become Ready")
			Expect(utils.WaitForESOResourceReady(ctx, dynamicClient,
				schema.GroupVersionResource{
					Group:    externalSecretsGroupName,
					Version:  v1alpha1APIVersion,
					Resource: PushSecretsKind,
				},
				testNamespace, pushSecretResourceName, time.Minute,
			)).To(Succeed())

			By("Creating ExternalSecret")
			loader.CreateFromFile(assetFunc, externalSecretFile, testNamespace)
			defer loader.DeleteFromFile(testassets.ReadFile, externalSecretFile, testNamespace)

			By("Waiting for ExternalSecret to become Ready")
			Expect(utils.WaitForESOResourceReady(ctx, dynamicClient,
				schema.GroupVersionResource{
					Group:    externalSecretsGroupName,
					Version:  v1APIVersion,
					Resource: externalSecretsKind,
				},
				testNamespace, externalSecretResourceName, time.Minute,
			)).To(Succeed())

			By("Waiting for target secret to be created with expected data")
			Eventually(func(g Gomega) {
				secret, err := loader.KubeClient.CoreV1().Secrets(testNamespace).Get(ctx, secretResourceName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "should get %s from namespace %s", secretResourceName, testNamespace)

				val, ok := secret.Data[keyNameInSecret]
				g.Expect(ok).To(BeTrue(), "%s should be present in secret %s", keyNameInSecret, secret.Name)

				g.Expect(val).To(Equal(expectedSecretValue), "%s does not match expected value", keyNameInSecret)
			}, time.Minute, 10*time.Second).Should(Succeed())
		})
	})

	Context("Environment Variables", Label("Platform:Generic", "Feature:OverrideEnv"), func() {
		// Map component names to deployment names and target container names
		componentToDeployment := map[string]string{
			"ExternalSecretsCoreController": externalsecrets.OperandCoreControllerDeployment,
			"Webhook":                       externalsecrets.OperandWebhookDeployment,
			"CertController":                externalsecrets.OperandCertControllerDeployment,
		}
		componentToContainer := map[string]string{
			"ExternalSecretsCoreController": externalsecrets.OperandCoreControllerContainer,
			"Webhook":                       externalsecrets.OperandWebhookContainer,
			"CertController":                externalsecrets.OperandCertControllerContainer,
		}

		// Define test env vars
		envConfigs := []operatorv1alpha1.ComponentConfig{
			{
				ComponentName: "ExternalSecretsCoreController",
				OverrideEnv: []corev1.EnvVar{
					{Name: "LOG_LEVEL", Value: "debug"},
					{Name: "TEST_CONTROLLER_VAR", Value: "controller-value"},
				},
			},
			{
				ComponentName: "Webhook",
				OverrideEnv: []corev1.EnvVar{
					{Name: "TLS_MIN_VERSION", Value: "1.2"},
					{Name: "TEST_WEBHOOK_VAR", Value: "webhook-value"},
				},
			},
			{
				ComponentName: "CertController",
				OverrideEnv: []corev1.EnvVar{
					{Name: "TEST_CERT_VAR", Value: "cert-value"},
					{Name: "FOO", Value: "bar"},
				},
			},
		}

		It("should set custom environment variables for all component deployments", func() {
			esc := &operatorv1alpha1.ExternalSecretsConfig{}
			Expect(runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, esc)).To(Succeed())
			applicableEnvConfigs := componentConfigsForESC(esc, envConfigs)

			By("Updating ExternalSecretsConfig with custom env vars")
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				existingCR := &operatorv1alpha1.ExternalSecretsConfig{}
				if err := runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, existingCR); err != nil {
					return err
				}

				updatedCR := existingCR.DeepCopy()
				updatedCR.Spec.ControllerConfig.ComponentConfigs = applicableEnvConfigs

				return runtimeClient.Update(ctx, updatedCR)
			})
			Expect(err).NotTo(HaveOccurred(), "should update ExternalSecretsConfig with custom env vars")

			By("Waiting for pods to be ready after config update")
			Expect(utils.VerifyOperandPodsReady(ctx, clientset, operandNamespace, esc)).To(Succeed())

			for _, config := range applicableEnvConfigs {
				By(fmt.Sprintf("Verifying custom environment variables in %s deployment", config.ComponentName))

				deploymentName := componentToDeployment[string(config.ComponentName)]
				targetContainerName := componentToContainer[string(config.ComponentName)]
				Eventually(func(g Gomega) {
					deployment, err := clientset.AppsV1().Deployments(operandNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred(), "should get %s deployment", deploymentName)

					// Verify env vars on the target container specifically
					for _, container := range deployment.Spec.Template.Spec.Containers {
						if container.Name != targetContainerName {
							continue
						}
						envMap := make(map[string]string)
						for _, env := range container.Env {
							envMap[env.Name] = env.Value
						}
						for _, expectedEnv := range config.OverrideEnv {
							g.Expect(envMap).To(HaveKeyWithValue(expectedEnv.Name, expectedEnv.Value),
								"container %s in %s should have env var %s=%s", targetContainerName, deploymentName, expectedEnv.Name, expectedEnv.Value)
						}
					}
				}, time.Minute, 5*time.Second).Should(Succeed(), "env vars should be set for %s", config.ComponentName)
			}
		})

		It("should remove custom environment variables when config is cleared", func() {
			esc := &operatorv1alpha1.ExternalSecretsConfig{}
			Expect(runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, esc)).To(Succeed())
			applicableEnvConfigs := componentConfigsForESC(esc, envConfigs)

			By("Removing custom env vars from ExternalSecretsConfig")
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				existingCR := &operatorv1alpha1.ExternalSecretsConfig{}
				if err := runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, existingCR); err != nil {
					return err
				}

				updatedCR := existingCR.DeepCopy()
				updatedCR.Spec.ControllerConfig.ComponentConfigs = nil

				return runtimeClient.Update(ctx, updatedCR)
			})
			Expect(err).NotTo(HaveOccurred(), "should update ExternalSecretsConfig to remove custom env vars")

			By("Waiting for pods to be ready after config update")
			Expect(utils.VerifyOperandPodsReady(ctx, clientset, operandNamespace, esc)).To(Succeed())

			for _, config := range applicableEnvConfigs {
				By(fmt.Sprintf("Verifying custom environment variables removed from %s deployment", config.ComponentName))

				deploymentName := componentToDeployment[string(config.ComponentName)]
				targetContainerName := componentToContainer[string(config.ComponentName)]
				Eventually(func(g Gomega) {
					deployment, err := clientset.AppsV1().Deployments(operandNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred(), "should get %s deployment", deploymentName)

					// Verify env vars are removed from the target container
					for _, container := range deployment.Spec.Template.Spec.Containers {
						if container.Name != targetContainerName {
							continue
						}
						envNames := make(map[string]bool)
						for _, env := range container.Env {
							envNames[env.Name] = true
						}
						for _, expectedEnv := range config.OverrideEnv {
							g.Expect(envNames).NotTo(HaveKey(expectedEnv.Name),
								"container %s in %s should not have env var %s after removal", targetContainerName, deploymentName, expectedEnv.Name)
						}
					}
				}, time.Minute, 5*time.Second).Should(Succeed(), "env vars should be removed from %s", config.ComponentName)
			}
		})
	})

	Context("Deployment Revision History Limit", Label("Platform:Generic", "Feature:RevisionHistoryLimit"), func() {
		It("should use default revisionHistoryLimit when not configured", func() {
			esc := &operatorv1alpha1.ExternalSecretsConfig{}
			Expect(runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, esc)).To(Succeed())

			By("Verifying default revisionHistoryLimit (10) for ExternalSecretsCoreController deployment")
			Eventually(func(g Gomega) {
				deployment, err := clientset.AppsV1().Deployments(operandNamespace).Get(ctx, externalsecrets.OperandCoreControllerDeployment, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "should get %s deployment", externalsecrets.OperandCoreControllerDeployment)
				g.Expect(deployment.Spec.RevisionHistoryLimit).NotTo(BeNil(), "revisionHistoryLimit should be set")
				g.Expect(*deployment.Spec.RevisionHistoryLimit).To(Equal(int32(10)), "revisionHistoryLimit should default to 10 when not configured")
				hasArg, found := deploymentContainerHasArg(deployment, externalsecrets.OperandCoreControllerContainer, externalsecrets.UnsafeAllowGenericTargetsArg)
				g.Expect(found).To(BeTrue(), "core controller container should exist")
				g.Expect(hasArg).To(BeFalse(), "unsafe-allow-generic-targets should not be set when the feature is not configured")
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Verifying default revisionHistoryLimit (10) for Webhook deployment")
			Eventually(func(g Gomega) {
				deployment, err := clientset.AppsV1().Deployments(operandNamespace).Get(ctx, externalsecrets.OperandWebhookDeployment, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "should get %s deployment", externalsecrets.OperandWebhookDeployment)
				g.Expect(deployment.Spec.RevisionHistoryLimit).NotTo(BeNil(), "revisionHistoryLimit should be set")
				g.Expect(*deployment.Spec.RevisionHistoryLimit).To(Equal(int32(10)), "revisionHistoryLimit should default to 10 when not configured")
			}, time.Minute, 5*time.Second).Should(Succeed())

			if utils.IsCertControllerExpected(esc) {
				By("Verifying default revisionHistoryLimit (10) for CertController deployment")
				Eventually(func(g Gomega) {
					deployment, err := clientset.AppsV1().Deployments(operandNamespace).Get(ctx, externalsecrets.OperandCertControllerDeployment, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred(), "should get %s deployment", externalsecrets.OperandCertControllerDeployment)
					g.Expect(deployment.Spec.RevisionHistoryLimit).NotTo(BeNil(), "revisionHistoryLimit should be set")
					g.Expect(*deployment.Spec.RevisionHistoryLimit).To(Equal(int32(10)), "revisionHistoryLimit should default to 10 when not configured")
				}, time.Minute, 5*time.Second).Should(Succeed())
			}
		})

		It("should set custom revisionHistoryLimit for all component deployments", func() {
			const (
				controllerLimit     = int32(3)
				webhookLimit        = int32(5)
				certControllerLimit = int32(2)
			)

			By("Updating the ExternalSecretsConfig with custom revision history limits")
			loader.DeleteFromFile(testassets.ReadFile, externalSecretsFile, "")
			loader.CreateFromFile(testassets.ReadFile, externalSecretsFileWithRevisionLimit, "")
			defer func() {
				loader.DeleteFromFile(testassets.ReadFile, externalSecretsFileWithRevisionLimit, "")
				loader.CreateFromFile(testassets.ReadFile, externalSecretsFile, "")
			}()

			esc := &operatorv1alpha1.ExternalSecretsConfig{}
			Expect(runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, esc)).To(Succeed())

			By("Waiting for pods to be ready after config update")
			Expect(utils.VerifyOperandPodsReady(ctx, clientset, operandNamespace, esc)).To(Succeed())

			By("Verifying custom revisionHistoryLimit (3) for ExternalSecretsCoreController deployment")
			Eventually(func(g Gomega) {
				deployment, err := clientset.AppsV1().Deployments(operandNamespace).Get(ctx, externalsecrets.OperandCoreControllerDeployment, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "should get %s deployment", externalsecrets.OperandCoreControllerDeployment)
				g.Expect(deployment.Spec.RevisionHistoryLimit).NotTo(BeNil(), "revisionHistoryLimit should be set")
				g.Expect(*deployment.Spec.RevisionHistoryLimit).To(Equal(controllerLimit), "revisionHistoryLimit should be %d for controller", controllerLimit)
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Verifying custom revisionHistoryLimit (5) for Webhook deployment")
			Eventually(func(g Gomega) {
				deployment, err := clientset.AppsV1().Deployments(operandNamespace).Get(ctx, externalsecrets.OperandWebhookDeployment, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "should get %s deployment", externalsecrets.OperandWebhookDeployment)
				g.Expect(deployment.Spec.RevisionHistoryLimit).NotTo(BeNil(), "revisionHistoryLimit should be set")
				g.Expect(*deployment.Spec.RevisionHistoryLimit).To(Equal(webhookLimit), "revisionHistoryLimit should be %d for webhook", webhookLimit)
			}, time.Minute, 5*time.Second).Should(Succeed())

			if utils.IsCertControllerExpected(esc) {
				By("Verifying custom revisionHistoryLimit (2) for CertController deployment")
				Eventually(func(g Gomega) {
					deployment, err := clientset.AppsV1().Deployments(operandNamespace).Get(ctx, externalsecrets.OperandCertControllerDeployment, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred(), "should get %s deployment", externalsecrets.OperandCertControllerDeployment)
					g.Expect(deployment.Spec.RevisionHistoryLimit).NotTo(BeNil(), "revisionHistoryLimit should be set")
					g.Expect(*deployment.Spec.RevisionHistoryLimit).To(Equal(certControllerLimit), "revisionHistoryLimit should be %d for cert-controller", certControllerLimit)
				}, time.Minute, 5*time.Second).Should(Succeed())
			}
		})
	})

	Context("UnsafeAllowGenericTargets feature", Label("Platform:Generic", "Feature:UnsafeAllowGenericTargets"), func() {
		var (
			originalFeatures []operatorv1alpha1.Feature
			hadFeatures      bool
		)

		updateFeatureMode := func(mode operatorv1alpha1.Mode) {
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				esm := &operatorv1alpha1.ExternalSecretsManager{}
				if err := runtimeClient.Get(ctx, client.ObjectKey{Name: "cluster"}, esm); err != nil {
					return err
				}
				updated := esm.DeepCopy()
				updated.Spec.Features = []operatorv1alpha1.Feature{
					{Name: operatorv1alpha1.UnsafeAllowGenericTargets, Mode: mode},
				}
				return runtimeClient.Update(ctx, updated)
			})
			Expect(err).NotTo(HaveOccurred(), "should update ExternalSecretsManager feature mode to %q", mode)
		}

		clearFeatures := func() {
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				esm := &operatorv1alpha1.ExternalSecretsManager{}
				if err := runtimeClient.Get(ctx, client.ObjectKey{Name: "cluster"}, esm); err != nil {
					return err
				}
				updated := esm.DeepCopy()
				updated.Spec.Features = nil
				return runtimeClient.Update(ctx, updated)
			})
			Expect(err).NotTo(HaveOccurred(), "should clear ExternalSecretsManager features")
		}

		waitForCoreControllerArg := func(present bool) {
			Eventually(func(g Gomega) {
				deployment, err := clientset.AppsV1().Deployments(operandNamespace).Get(ctx, externalsecrets.OperandCoreControllerDeployment, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "should get external-secrets deployment")
				hasArg, found := deploymentContainerHasArg(deployment, externalsecrets.OperandCoreControllerContainer, externalsecrets.UnsafeAllowGenericTargetsArg)
				g.Expect(found).To(BeTrue(), "core controller container should exist")
				if present {
					g.Expect(hasArg).To(BeTrue(), "core controller deployment should include %q", externalsecrets.UnsafeAllowGenericTargetsArg)
				} else {
					g.Expect(hasArg).To(BeFalse(), "core controller deployment should not include %q", externalsecrets.UnsafeAllowGenericTargetsArg)
				}
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		}

		BeforeAll(func() {
			esm := &operatorv1alpha1.ExternalSecretsManager{}
			Expect(runtimeClient.Get(ctx, client.ObjectKey{Name: "cluster"}, esm)).To(Succeed(), "should get ExternalSecretsManager")
			if len(esm.Spec.Features) > 0 {
				hadFeatures = true
				originalFeatures = append([]operatorv1alpha1.Feature(nil), esm.Spec.Features...)
			}
		})

		AfterAll(func() {
			By("Restoring original ExternalSecretsManager features")
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				esm := &operatorv1alpha1.ExternalSecretsManager{}
				if err := runtimeClient.Get(ctx, client.ObjectKey{Name: "cluster"}, esm); err != nil {
					return err
				}
				updated := esm.DeepCopy()
				if hadFeatures {
					updated.Spec.Features = append([]operatorv1alpha1.Feature(nil), originalFeatures...)
				} else {
					updated.Spec.Features = nil
				}
				return runtimeClient.Update(ctx, updated)
			})
			Expect(err).NotTo(HaveOccurred(), "should restore ExternalSecretsManager features")

			Expect(utils.VerifyPodsReadyByPrefix(ctx, clientset, operandNamespace, []string{
				operandCoreControllerPodPrefix,
				operandCertControllerPodPrefix,
				operandWebhookPodPrefix,
			})).To(Succeed(), "operand pods should be ready after restoring ExternalSecretsManager features")
		})

		It("should add unsafe-allow-generic-targets arg to the core controller when feature is enabled", func() {
			By("Enabling UnsafeAllowGenericTargets on ExternalSecretsManager")
			updateFeatureMode(operatorv1alpha1.Enabled)

			By("Waiting for operand pods to be ready after feature enable")
			Expect(utils.VerifyPodsReadyByPrefix(ctx, clientset, operandNamespace, []string{
				operandCoreControllerPodPrefix,
				operandCertControllerPodPrefix,
				operandWebhookPodPrefix,
			})).To(Succeed(), "operand pods should be ready after enabling UnsafeAllowGenericTargets")

			By("Verifying the core controller deployment includes the feature arg")
			waitForCoreControllerArg(true)

			By("Verifying the feature arg is not added to webhook or cert-controller deployments")
			for _, tc := range []struct {
				deploymentName string
				containerName  string
			}{
				{deploymentName: externalsecrets.OperandWebhookDeployment, containerName: externalsecrets.OperandWebhookContainer},
				{deploymentName: externalsecrets.OperandCertControllerDeployment, containerName: externalsecrets.OperandCertControllerContainer},
			} {
				deployment, err := clientset.AppsV1().Deployments(operandNamespace).Get(ctx, tc.deploymentName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred(), "should get %s deployment", tc.deploymentName)
				hasArg, found := deploymentContainerHasArg(deployment, tc.containerName, externalsecrets.UnsafeAllowGenericTargetsArg)
				Expect(found).To(BeTrue(), "%s container %q should exist", tc.deploymentName, tc.containerName)
				Expect(hasArg).To(BeFalse(),
					"%s deployment should not include %q", tc.deploymentName, externalsecrets.UnsafeAllowGenericTargetsArg)
			}
		})

		It("should remove unsafe-allow-generic-targets arg when feature is disabled", func() {
			By("Disabling UnsafeAllowGenericTargets on ExternalSecretsManager")
			updateFeatureMode(operatorv1alpha1.Disabled)

			By("Waiting for operand pods to be ready after feature disable")
			Expect(utils.VerifyPodsReadyByPrefix(ctx, clientset, operandNamespace, []string{
				operandCoreControllerPodPrefix,
				operandCertControllerPodPrefix,
				operandWebhookPodPrefix,
			})).To(Succeed(), "operand pods should be ready after disabling UnsafeAllowGenericTargets")

			By("Verifying the core controller deployment no longer includes the feature arg")
			waitForCoreControllerArg(false)
		})

		It("should remove unsafe-allow-generic-targets arg when feature is cleared from ExternalSecretsManager", func() {
			By("Clearing features from ExternalSecretsManager")
			clearFeatures()

			By("Waiting for operand pods to be ready after feature removal")
			Expect(utils.VerifyPodsReadyByPrefix(ctx, clientset, operandNamespace, []string{
				operandCoreControllerPodPrefix,
				operandCertControllerPodPrefix,
				operandWebhookPodPrefix,
			})).To(Succeed(), "operand pods should be ready after clearing ExternalSecretsManager features")

			By("Verifying the core controller deployment no longer includes the feature arg")
			waitForCoreControllerArg(false)
		})
	})

	Context("Annotations", Label("Platform:Generic", "Feature:CustomAnnotations"), func() {
		It("should apply and remove custom annotations to created resources", func() {
			// Define test annotations
			testAnnotations := map[string]string{
				"example.com/custom-annotation": "test-value",
				"mycompany.io/owner":            "platform-team",
			}

			// Capture original annotations so we can restore them and avoid test pollution
			existingCR := &operatorv1alpha1.ExternalSecretsConfig{}
			Expect(runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, existingCR)).To(Succeed(), "should get ExternalSecretsConfig to capture initial state")
			originalAnnotations := maps.Clone(existingCR.Spec.ControllerConfig.Annotations)

			defer func() {
				By("Restoring original annotations on ExternalSecretsConfig CR")
				err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					currentCR := &operatorv1alpha1.ExternalSecretsConfig{}
					if err := runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, currentCR); err != nil {
						return err
					}
					currentCR.Spec.ControllerConfig.Annotations = originalAnnotations
					return runtimeClient.Update(ctx, currentCR)
				})
				Expect(err).NotTo(HaveOccurred(), "should restore annotations on ExternalSecretsConfig")
			}()

			By("Updating ExternalSecretsConfig with custom annotations")
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				existingCR := &operatorv1alpha1.ExternalSecretsConfig{}
				if err := runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, existingCR); err != nil {
					return err
				}

				updatedCR := existingCR.DeepCopy()
				merged := make(map[string]string)
				if originalAnnotations != nil {
					maps.Copy(merged, originalAnnotations)
				}
				maps.Copy(merged, testAnnotations)
				updatedCR.Spec.ControllerConfig.Annotations = merged

				return runtimeClient.Update(ctx, updatedCR)
			})
			Expect(err).NotTo(HaveOccurred(), "should update ExternalSecretsConfig with annotations")

			By("Waiting for external-secrets operand pods to be ready")
			Expect(utils.VerifyOperandPodsReady(ctx, clientset, operandNamespace, existingCR)).To(Succeed())

			// Verify annotations are applied to each resource type
			for _, resourceType := range getResourceTypesToVerify() {
				By(fmt.Sprintf("Verifying annotations are applied to %s resources", resourceType.name))
				Eventually(func(g Gomega) {
					objects, err := resourceType.listFunc(ctx, clientset, operandNamespace, g)
					g.Expect(err).NotTo(HaveOccurred(), "should list %s in %s namespace", resourceType.name, operandNamespace)

					for _, obj := range objects {
						if !strings.HasPrefix(obj.GetName(), "external-secrets") {
							continue
						}

						annotations := obj.GetAnnotations()
						for key, value := range testAnnotations {
							g.Expect(annotations).To(HaveKeyWithValue(key, value),
								"%s %s should have annotation %s=%s", resourceType.name, obj.GetName(), key, value)
						}

						if resourceType.checkPodSpec {
							deployment := asDeployment(obj)
							templateAnnotations := deployment.Spec.Template.Annotations
							for key, value := range testAnnotations {
								g.Expect(templateAnnotations).To(HaveKeyWithValue(key, value),
									"deployment %s pod template should have annotation %s=%s", deployment.Name, key, value)
							}
						}
					}
				}, 2*time.Minute, 5*time.Second).Should(Succeed())
			}

			By("Removing test annotations from ExternalSecretsConfig CR")
			err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				currentCR := &operatorv1alpha1.ExternalSecretsConfig{}
				if err := runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, currentCR); err != nil {
					return err
				}
				for key := range testAnnotations {
					delete(currentCR.Spec.ControllerConfig.Annotations, key)
				}
				return runtimeClient.Update(ctx, currentCR)
			})
			Expect(err).NotTo(HaveOccurred(), "should remove test annotations from ExternalSecretsConfig")

			// Verify annotations are removed from each resource type
			for _, resourceType := range getResourceTypesToVerify() {
				By(fmt.Sprintf("Verifying annotations are removed from %s resources", resourceType.name))
				Eventually(func(g Gomega) {
					objects, err := resourceType.listFunc(ctx, clientset, operandNamespace, g)
					g.Expect(err).NotTo(HaveOccurred(), "should list %s in %s namespace", resourceType.name, operandNamespace)

					for _, obj := range objects {
						if !strings.HasPrefix(obj.GetName(), "external-secrets") {
							continue
						}

						annotations := obj.GetAnnotations()
						for key := range testAnnotations {
							g.Expect(annotations).NotTo(HaveKey(key),
								"%s %s should NOT have annotation %s after removal", resourceType.name, obj.GetName(), key)
						}

						if resourceType.checkPodSpec {
							deployment := asDeployment(obj)
							templateAnnotations := deployment.Spec.Template.Annotations
							for key := range testAnnotations {
								g.Expect(templateAnnotations).NotTo(HaveKey(key),
									"deployment %s pod template should NOT have annotation %s after removal", deployment.Name, key)
							}
						}
					}
				}, 2*time.Minute, 5*time.Second).Should(Succeed())
			}
		})

		It("should not allow updating annotations with reserved domain prefix", func() {
			By("Getting the existing ExternalSecretsConfig CR")
			existingCR := &operatorv1alpha1.ExternalSecretsConfig{}
			err := runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, existingCR)
			Expect(err).NotTo(HaveOccurred(), "should get ExternalSecretsConfig CR")

			By("Attempting to update with a reserved domain annotation")
			updatedCR := existingCR.DeepCopy()
			if updatedCR.Spec.ControllerConfig.Annotations == nil {
				updatedCR.Spec.ControllerConfig.Annotations = make(map[string]string)
			}

			// Add two reserved annotations that should be rejected
			updatedCR.Spec.ControllerConfig.Annotations["deployment.kubernetes.io/revision"] = "9"
			updatedCR.Spec.ControllerConfig.Annotations["k8s.io/not-allowed"] = "denied"

			err = runtimeClient.Update(ctx, updatedCR)
			Expect(err).To(HaveOccurred(), "update with reserved domain annotations should fail")
		})
	})

	Context("Static Network Policy Naming", Label("Platform:Generic", "Feature:NetworkPolicy"), func() {
		listManagedNetworkPolicies := func(ctx context.Context, namespace string) ([]networkingv1.NetworkPolicy, error) {
			npList, err := clientset.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("%s=%s,%s=%s", managedByLabel, managedByValue, partOfLabel, managedByValue),
			})
			if err != nil {
				return nil, err
			}
			return npList.Items, nil
		}

		It("should create all static network policies with eso-sys- prefix", func() {
			By("Listing managed network policies in operand namespace")
			Eventually(func(g Gomega) {
				nps, err := listManagedNetworkPolicies(ctx, operandNamespace)
				g.Expect(err).NotTo(HaveOccurred())

				npNames := make(map[string]bool)
				for _, np := range nps {
					npNames[np.Name] = true
				}

				for _, expected := range expectedStaticNPNames {
					g.Expect(npNames).To(HaveKey(expected),
						"static network policy %s should exist", expected)
				}
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should not have any unprefixed legacy network policies", func() {
			By("Verifying no unprefixed network policies exist")
			legacyNames := []string{
				"deny-all-traffic",
				"allow-api-server-egress-for-main-controller",
				"allow-api-server-egress-for-webhook",
				"allow-api-server-egress-for-cert-controller",
				"allow-to-dns",
			}

			Eventually(func(g Gomega) {
				nps, err := listManagedNetworkPolicies(ctx, operandNamespace)
				g.Expect(err).NotTo(HaveOccurred())

				npNames := make(map[string]bool)
				for _, np := range nps {
					npNames[np.Name] = true
				}

				for _, legacy := range legacyNames {
					g.Expect(npNames).NotTo(HaveKey(legacy),
						"legacy unprefixed network policy %s should have been cleaned up", legacy)
				}
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		// TODO: Remove this test case after 3 releases(in v1.5.0) once the migration from
		// unprefixed to eso-sys-/eso-user- network policy names is no longer needed.
		It("should set the skip-np-cleanup-check annotation on ExternalSecretsConfig after migration",
			Label("Feature:Upgrade"), func() {
				By("Verifying the cleanup annotation is present on the CR")
				Eventually(func(g Gomega) {
					esc := &operatorv1alpha1.ExternalSecretsConfig{}
					g.Expect(runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, esc)).To(Succeed())

					annotations := esc.GetAnnotations()
					g.Expect(annotations).To(HaveKeyWithValue(
						"externalsecretsconfig.operator.openshift.io/skip-np-cleanup-check", "true"),
						"ExternalSecretsConfig should have the skip-np-cleanup-check annotation after migration cleanup")
				}, 2*time.Minute, 5*time.Second).Should(Succeed())

				By("Verifying all managed network policies use the eso-sys- or eso-user- prefix")
				nps, err := listManagedNetworkPolicies(ctx, operandNamespace)
				Expect(err).NotTo(HaveOccurred())

				for _, np := range nps {
					Expect(np.Name).To(SatisfyAny(
						HavePrefix("eso-sys-"),
						HavePrefix("eso-user-"),
					), "managed network policy %s should have eso-sys- or eso-user- prefix after migration", np.Name)
				}
			})
	})

	Context("Custom Network Policy Naming", Label("Platform:Generic", "Feature:NetworkPolicy"), func() {
		It("should create custom network policies with eso-user- prefix from CR spec", func() {
			const testPolicyName = "e2e-test-custom-np"
			expectedNPName := userNPPrefix + testPolicyName

			// networkPolicies name/componentName are immutable (CEL); entries cannot be removed after creation.
			// Add the test policy if missing, then verify the operator materializes it in the operand namespace.
			By("Adding a custom network policy with a dummy egress port to ExternalSecretsConfig")
			tcp := corev1.ProtocolTCP
			port8443 := intstr.FromInt32(8443)
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				currentCR := &operatorv1alpha1.ExternalSecretsConfig{}
				if err := runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, currentCR); err != nil {
					return err
				}
				for _, np := range currentCR.Spec.ControllerConfig.NetworkPolicies {
					if np.Name == testPolicyName {
						return nil
					}
				}
				currentCR.Spec.ControllerConfig.NetworkPolicies = append(
					currentCR.Spec.ControllerConfig.NetworkPolicies,
					operatorv1alpha1.NetworkPolicy{
						Name:          testPolicyName,
						ComponentName: operatorv1alpha1.CoreController,
						Egress: []networkingv1.NetworkPolicyEgressRule{
							{
								Ports: []networkingv1.NetworkPolicyPort{
									{Protocol: &tcp, Port: &port8443},
								},
							},
						},
					},
				)
				return runtimeClient.Update(ctx, currentCR)
			})
			Expect(err).NotTo(HaveOccurred(), "should add custom network policy to ExternalSecretsConfig")

			By("Waiting for custom network policy to be created with eso-user- prefix in operand namespace")
			Eventually(func(g Gomega) {
				np, err := clientset.NetworkingV1().NetworkPolicies(operandNamespace).Get(ctx, expectedNPName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "custom network policy %s should exist in %s", expectedNPName, operandNamespace)
				g.Expect(np.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeEgress))
				g.Expect(np.Spec.Egress).To(HaveLen(1))
				g.Expect(np.Spec.Egress[0].Ports).To(HaveLen(1))
				g.Expect(np.Spec.Egress[0].Ports[0].Port).NotTo(BeNil())
				g.Expect(np.Spec.Egress[0].Ports[0].Port.IntVal).To(Equal(int32(8443)))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})
	})

	// Tests labeled with Feature:Proxy should only be run when a cluster-wide
	// OpenShift egress proxy is already configured via proxy.config.openshift.io/cluster object
	Context("Proxy Egress Network Policy", Label("Platform:Generic", "Feature:Proxy"), func() {
		var clusterProxyPorts []int32
		var originalProxy *operatorv1alpha1.ProxyConfig

		BeforeAll(func() {
			By("Capturing original proxy configuration from ExternalSecretsConfig")
			esc := &operatorv1alpha1.ExternalSecretsConfig{}
			Expect(runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, esc)).To(Succeed())
			if esc.Spec.ApplicationConfig.Proxy != nil {
				originalProxy = esc.Spec.ApplicationConfig.Proxy.DeepCopy()
			}

			By("Checking cluster-wide proxy configuration exists")
			proxyGVR := schema.GroupVersionResource{
				Group:    "config.openshift.io",
				Version:  "v1",
				Resource: "proxies",
			}
			proxyCR, err := dynamicClient.Resource(proxyGVR).Get(ctx, common.ExternalSecretsConfigObjectName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "should be able to read proxies.config.openshift.io/cluster CR")

			proxyStatus, _, _ := unstructured.NestedMap(proxyCR.Object, "status")
			httpProxy, _, _ := unstructured.NestedString(proxyCR.Object, "status", "httpProxy")
			httpsProxy, _, _ := unstructured.NestedString(proxyCR.Object, "status", "httpsProxy")
			noProxy, _, _ := unstructured.NestedString(proxyCR.Object, "status", "noProxy")
			Expect(proxyStatus).NotTo(BeNil(), "proxy CR status should exist")
			Expect(httpProxy != "" || httpsProxy != "" || noProxy != "").To(BeTrue(),
				"at least one proxy setting (httpProxy, httpsProxy, noProxy) should be configured in the cluster-wide Proxy CR")

			clusterProxyPorts = expectedProxyPorts(httpsProxy, httpProxy)
		})

		AfterEach(func() {
			By("Restoring original proxy configuration on ExternalSecretsConfig")
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				currentCR := &operatorv1alpha1.ExternalSecretsConfig{}
				if err := runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, currentCR); err != nil {
					return err
				}
				currentCR.Spec.ApplicationConfig.Proxy = originalProxy
				return runtimeClient.Update(ctx, currentCR)
			})
			Expect(err).NotTo(HaveOccurred(), "should restore original proxy config")
		})

		// Cluster-wide proxy configuration consumed via OLM env vars.
		It("should create proxy egress policy when configured with Managed provisioning", func() {
			By("Setting proxy configuration with Managed network policy provisioning")
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				currentCR := &operatorv1alpha1.ExternalSecretsConfig{}
				if err := runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, currentCR); err != nil {
					return err
				}
				currentCR.Spec.ApplicationConfig.Proxy = &operatorv1alpha1.ProxyConfig{
					NetworkPolicyProvisioning: operatorv1alpha1.ManagementStateManaged,
				}
				return runtimeClient.Update(ctx, currentCR)
			})
			Expect(err).NotTo(HaveOccurred(), "should set proxy config with Managed provisioning")

			By("Waiting for proxy egress network policy to be created with correct port(s)")
			Eventually(func(g Gomega) {
				np, err := clientset.NetworkingV1().NetworkPolicies(operandNamespace).Get(ctx, npProxyEgressPolicyName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "proxy egress policy %s should be created", npProxyEgressPolicyName)
				g.Expect(np.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeEgress))
				g.Expect(np.Spec.Egress).NotTo(BeEmpty(), "egress rules should be configured")
				g.Expect(np.Spec.Egress[0].Ports).To(HaveLen(len(clusterProxyPorts)),
					"egress port count should match expected proxy ports")
				for i, wantPort := range clusterProxyPorts {
					g.Expect(np.Spec.Egress[0].Ports[i].Port.IntVal).To(Equal(wantPort),
						"egress port[%d] should match the cluster proxy URL port", i)
				}
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		// Cluster-wide proxy configuration consumed via OLM env vars.
		It("should remove proxy egress policy when switching from Managed to Unmanaged", func() {
			By("Setting proxy configuration with Managed provisioning first")
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				currentCR := &operatorv1alpha1.ExternalSecretsConfig{}
				if err := runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, currentCR); err != nil {
					return err
				}
				currentCR.Spec.ApplicationConfig.Proxy = &operatorv1alpha1.ProxyConfig{
					NetworkPolicyProvisioning: operatorv1alpha1.ManagementStateManaged,
				}
				return runtimeClient.Update(ctx, currentCR)
			})
			Expect(err).NotTo(HaveOccurred(), "should set proxy config with Managed provisioning")

			By("Waiting for proxy egress policy to be created")
			Eventually(func(g Gomega) {
				_, err := clientset.NetworkingV1().NetworkPolicies(operandNamespace).Get(ctx, npProxyEgressPolicyName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "proxy egress policy should be created")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("Switching to Unmanaged provisioning")
			err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				currentCR := &operatorv1alpha1.ExternalSecretsConfig{}
				if err := runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, currentCR); err != nil {
					return err
				}
				currentCR.Spec.ApplicationConfig.Proxy = &operatorv1alpha1.ProxyConfig{
					NetworkPolicyProvisioning: operatorv1alpha1.ManagementStateUnmanaged,
				}
				return runtimeClient.Update(ctx, currentCR)
			})
			Expect(err).NotTo(HaveOccurred(), "should switch to Unmanaged provisioning")

			By("Waiting for proxy egress policy to be removed")
			Eventually(func(g Gomega) {
				_, err := clientset.NetworkingV1().NetworkPolicies(operandNamespace).Get(ctx, npProxyEgressPolicyName, metav1.GetOptions{})
				g.Expect(k8serrors.IsNotFound(err)).To(BeTrue(), "proxy egress policy should be removed after switching to Unmanaged")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		// Proxy configured by user in ESConfig CR
		It("should not create proxy egress policy when provisioning is Unmanaged and user configured proxy", func() {
			By("Setting proxy configuration with Unmanaged network policy provisioning")
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				currentCR := &operatorv1alpha1.ExternalSecretsConfig{}
				if err := runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, currentCR); err != nil {
					return err
				}
				currentCR.Spec.ApplicationConfig.Proxy = &operatorv1alpha1.ProxyConfig{
					HTTPProxy:                 "http://proxy.example.com:3128",
					NetworkPolicyProvisioning: operatorv1alpha1.ManagementStateUnmanaged,
				}
				return runtimeClient.Update(ctx, currentCR)
			})
			Expect(err).NotTo(HaveOccurred(), "should set proxy config with Unmanaged provisioning")

			By("Verifying proxy egress policy is not created")
			Consistently(func(g Gomega) {
				_, err := clientset.NetworkingV1().NetworkPolicies(operandNamespace).Get(ctx, npProxyEgressPolicyName, metav1.GetOptions{})
				g.Expect(k8serrors.IsNotFound(err)).To(BeTrue(), "proxy egress policy should not exist when provisioning is Unmanaged")
			}, 30*time.Second, 5*time.Second).Should(Succeed())
		})

		// Proxy configured by user in ESConfig CR
		It("should create proxy egress policy when proxy is configured with Managed provisioning and user configured proxy", func() {
			By("Setting proxy configuration with Managed network policy provisioning")
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				currentCR := &operatorv1alpha1.ExternalSecretsConfig{}
				if err := runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, currentCR); err != nil {
					return err
				}
				currentCR.Spec.ApplicationConfig.Proxy = &operatorv1alpha1.ProxyConfig{
					HTTPProxy:                 "http://proxy.example.com:3128",
					NetworkPolicyProvisioning: operatorv1alpha1.ManagementStateManaged,
				}
				return runtimeClient.Update(ctx, currentCR)
			})
			Expect(err).NotTo(HaveOccurred(), "should set proxy config with Managed provisioning")

			By("Waiting for proxy egress network policy to be created with correct port")
			Eventually(func(g Gomega) {
				np, err := clientset.NetworkingV1().NetworkPolicies(operandNamespace).Get(ctx, npProxyEgressPolicyName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "proxy egress policy %s should be created", npProxyEgressPolicyName)
				g.Expect(np.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeEgress))
				g.Expect(np.Spec.Egress).NotTo(BeEmpty(), "egress rules should be configured")
				g.Expect(np.Spec.Egress[0].Ports).To(HaveLen(1), "single HTTP proxy should produce one egress port")
				g.Expect(np.Spec.Egress[0].Ports[0].Port.IntVal).To(Equal(int32(3128)),
					"egress port should match the explicit proxy URL port (3128)")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

	})

	// Context("Managed Annotation Restoration") tests that when a managed annotation is
	// externally removed from a resource, the operator detects the drift and restores it.
	// Resources intentionally excluded:
	//   - Deployment: Kubernetes adds deployment.kubernetes.io/revision on every rollout,
	//     so annotation events are suppressed to avoid infinite reconcile loops.
	Context("Managed Annotation Restoration", Ordered, Label("Platform:Generic", "Feature:CustomAnnotations"), func() {
		const (
			restorationAnnotationKey   = "example.com/restoration-test"
			restorationAnnotationValue = "managed-value"
		)

		BeforeAll(func() {
			By("Adding the restoration test annotation to ExternalSecretsConfig spec")
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				cr := &operatorv1alpha1.ExternalSecretsConfig{}
				if err := runtimeClient.Get(ctx, client.ObjectKey{Name: "cluster"}, cr); err != nil {
					return err
				}
				if cr.Spec.ControllerConfig.Annotations == nil {
					cr.Spec.ControllerConfig.Annotations = make(map[string]string)
				}
				cr.Spec.ControllerConfig.Annotations[restorationAnnotationKey] = restorationAnnotationValue
				return runtimeClient.Update(ctx, cr)
			})
			Expect(err).NotTo(HaveOccurred(), "should add restoration annotation to ExternalSecretsConfig")
		})

		AfterAll(func() {
			By("Removing the restoration test annotation from ExternalSecretsConfig spec")
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				cr := &operatorv1alpha1.ExternalSecretsConfig{}
				if err := runtimeClient.Get(ctx, client.ObjectKey{Name: "cluster"}, cr); err != nil {
					return err
				}
				delete(cr.Spec.ControllerConfig.Annotations, restorationAnnotationKey)
				return runtimeClient.Update(ctx, cr)
			})
			Expect(err).NotTo(HaveOccurred(), "should remove restoration annotation from ExternalSecretsConfig")

			By("Verifying ExternalSecretsConfig is Ready after annotation restoration tests")
			Expect(utils.WaitForExternalSecretsConfigReady(ctx, dynamicClient, "cluster", 2*time.Minute)).To(Succeed())

			By("Verifying operand pods are still ready after annotation restoration tests")
			Expect(utils.VerifyPodsReadyByPrefix(ctx, clientset, operandNamespace, []string{
				operandCoreControllerPodPrefix,
				operandCertControllerPodPrefix,
				operandWebhookPodPrefix,
			})).To(Succeed())
		})

		It("should restore a managed annotation on a ServiceAccount after external removal", func() {
			saName := "external-secrets"

			By("Verifying ServiceAccount has the managed annotation initially")
			Eventually(func(g Gomega) {
				sa, err := clientset.CoreV1().ServiceAccounts(operandNamespace).Get(ctx, saName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(sa.Annotations).To(HaveKeyWithValue(restorationAnnotationKey, restorationAnnotationValue))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("Externally removing the managed annotation from the ServiceAccount")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				sa, err := clientset.CoreV1().ServiceAccounts(operandNamespace).Get(ctx, saName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				delete(sa.Annotations, restorationAnnotationKey)
				_, err = clientset.CoreV1().ServiceAccounts(operandNamespace).Update(ctx, sa, metav1.UpdateOptions{})
				return err
			})).To(Succeed())

			By("Waiting for operator to restore the managed annotation")
			Eventually(func(g Gomega) {
				sa, err := clientset.CoreV1().ServiceAccounts(operandNamespace).Get(ctx, saName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(sa.Annotations).To(HaveKeyWithValue(restorationAnnotationKey, restorationAnnotationValue),
					"operator should restore annotation on ServiceAccount %s", saName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should restore a managed annotation on a Role after external removal", func() {
			roleName := "external-secrets-leaderelection"

			By("Verifying Role has the managed annotation initially")
			Eventually(func(g Gomega) {
				role, err := clientset.RbacV1().Roles(operandNamespace).Get(ctx, roleName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(role.Annotations).To(HaveKeyWithValue(restorationAnnotationKey, restorationAnnotationValue))
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Externally removing the managed annotation from the Role")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				role, err := clientset.RbacV1().Roles(operandNamespace).Get(ctx, roleName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				delete(role.Annotations, restorationAnnotationKey)
				_, err = clientset.RbacV1().Roles(operandNamespace).Update(ctx, role, metav1.UpdateOptions{})
				return err
			})).To(Succeed())

			By("Waiting for operator to restore the managed annotation")
			Eventually(func(g Gomega) {
				role, err := clientset.RbacV1().Roles(operandNamespace).Get(ctx, roleName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(role.Annotations).To(HaveKeyWithValue(restorationAnnotationKey, restorationAnnotationValue),
					"operator should restore annotation on Role %s", roleName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should restore a managed annotation on a RoleBinding after external removal", func() {
			roleBindingName := "external-secrets-leaderelection"

			By("Verifying RoleBinding has the managed annotation initially")
			Eventually(func(g Gomega) {
				rb, err := clientset.RbacV1().RoleBindings(operandNamespace).Get(ctx, roleBindingName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(rb.Annotations).To(HaveKeyWithValue(restorationAnnotationKey, restorationAnnotationValue))
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Externally removing the managed annotation from the RoleBinding")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				rb, err := clientset.RbacV1().RoleBindings(operandNamespace).Get(ctx, roleBindingName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				delete(rb.Annotations, restorationAnnotationKey)
				_, err = clientset.RbacV1().RoleBindings(operandNamespace).Update(ctx, rb, metav1.UpdateOptions{})
				return err
			})).To(Succeed())

			By("Waiting for operator to restore the managed annotation")
			Eventually(func(g Gomega) {
				rb, err := clientset.RbacV1().RoleBindings(operandNamespace).Get(ctx, roleBindingName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(rb.Annotations).To(HaveKeyWithValue(restorationAnnotationKey, restorationAnnotationValue),
					"operator should restore annotation on RoleBinding %s", roleBindingName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should restore a managed annotation on a ClusterRole after external removal", func() {
			clusterRoleName := "external-secrets-controller"

			By("Verifying ClusterRole has the managed annotation initially")
			Eventually(func(g Gomega) {
				cr, err := clientset.RbacV1().ClusterRoles().Get(ctx, clusterRoleName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Annotations).To(HaveKeyWithValue(restorationAnnotationKey, restorationAnnotationValue))
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Externally removing the managed annotation from the ClusterRole")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				cr, err := clientset.RbacV1().ClusterRoles().Get(ctx, clusterRoleName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				delete(cr.Annotations, restorationAnnotationKey)
				_, err = clientset.RbacV1().ClusterRoles().Update(ctx, cr, metav1.UpdateOptions{})
				return err
			})).To(Succeed())

			By("Waiting for operator to restore the managed annotation")
			Eventually(func(g Gomega) {
				cr, err := clientset.RbacV1().ClusterRoles().Get(ctx, clusterRoleName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Annotations).To(HaveKeyWithValue(restorationAnnotationKey, restorationAnnotationValue),
					"operator should restore annotation on ClusterRole %s", clusterRoleName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should restore a managed annotation on a ClusterRoleBinding after external removal", func() {
			clusterRoleBindingName := "external-secrets-controller"

			By("Verifying ClusterRoleBinding has the managed annotation initially")
			Eventually(func(g Gomega) {
				crb, err := clientset.RbacV1().ClusterRoleBindings().Get(ctx, clusterRoleBindingName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(crb.Annotations).To(HaveKeyWithValue(restorationAnnotationKey, restorationAnnotationValue))
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Externally removing the managed annotation from the ClusterRoleBinding")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				crb, err := clientset.RbacV1().ClusterRoleBindings().Get(ctx, clusterRoleBindingName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				delete(crb.Annotations, restorationAnnotationKey)
				_, err = clientset.RbacV1().ClusterRoleBindings().Update(ctx, crb, metav1.UpdateOptions{})
				return err
			})).To(Succeed())

			By("Waiting for operator to restore the managed annotation")
			Eventually(func(g Gomega) {
				crb, err := clientset.RbacV1().ClusterRoleBindings().Get(ctx, clusterRoleBindingName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(crb.Annotations).To(HaveKeyWithValue(restorationAnnotationKey, restorationAnnotationValue),
					"operator should restore annotation on ClusterRoleBinding %s", clusterRoleBindingName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should restore a managed annotation on a Service after external removal", func() {
			serviceName := "external-secrets-webhook"

			By("Verifying Service has the managed annotation initially")
			Eventually(func(g Gomega) {
				svc, err := clientset.CoreV1().Services(operandNamespace).Get(ctx, serviceName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(svc.Annotations).To(HaveKeyWithValue(restorationAnnotationKey, restorationAnnotationValue))
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Externally removing the managed annotation from the Service")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				svc, err := clientset.CoreV1().Services(operandNamespace).Get(ctx, serviceName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				delete(svc.Annotations, restorationAnnotationKey)
				_, err = clientset.CoreV1().Services(operandNamespace).Update(ctx, svc, metav1.UpdateOptions{})
				return err
			})).To(Succeed())

			By("Waiting for operator to restore the managed annotation")
			Eventually(func(g Gomega) {
				svc, err := clientset.CoreV1().Services(operandNamespace).Get(ctx, serviceName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(svc.Annotations).To(HaveKeyWithValue(restorationAnnotationKey, restorationAnnotationValue),
					"operator should restore annotation on Service %s", serviceName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should restore a managed annotation on a NetworkPolicy after external removal", func() {
			npName := "eso-sys-deny-all-traffic"

			By("Verifying NetworkPolicy has the managed annotation initially")
			Eventually(func(g Gomega) {
				np, err := clientset.NetworkingV1().NetworkPolicies(operandNamespace).Get(ctx, npName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(np.Annotations).To(HaveKeyWithValue(restorationAnnotationKey, restorationAnnotationValue))
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Externally removing the managed annotation from the NetworkPolicy")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				np, err := clientset.NetworkingV1().NetworkPolicies(operandNamespace).Get(ctx, npName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				delete(np.Annotations, restorationAnnotationKey)
				_, err = clientset.NetworkingV1().NetworkPolicies(operandNamespace).Update(ctx, np, metav1.UpdateOptions{})
				return err
			})).To(Succeed())

			By("Waiting for operator to restore the managed annotation")
			Eventually(func(g Gomega) {
				np, err := clientset.NetworkingV1().NetworkPolicies(operandNamespace).Get(ctx, npName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(np.Annotations).To(HaveKeyWithValue(restorationAnnotationKey, restorationAnnotationValue),
					"operator should restore annotation on NetworkPolicy %s", npName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})
	})

	// Context("Custom Labels") tests that labels added via esc.Spec.ControllerConfig.Labels
	// are propagated to all managed resources, and that removing a label from the spec
	// causes the operator to remove it from all resources. This exercises the full
	// label lifecycle across every resource type, including the co-managed Secret whose
	// metadata is patched via JSON Patch rather than a full update.
	Context("Custom Labels", Label("Platform:Generic", "Feature:CustomLabels"), func() {
		AfterEach(func() {
			By("Verifying ExternalSecretsConfig is Ready and not Degraded after label lifecycle test")
			Expect(utils.WaitForExternalSecretsConfigReady(ctx, dynamicClient, "cluster", 2*time.Minute)).To(Succeed(),
				"ExternalSecretsConfig should remain Ready after custom label add/remove")

			By("Verifying operand pods are still ready after label lifecycle test")
			Expect(utils.VerifyPodsReadyByPrefix(ctx, clientset, operandNamespace, []string{
				operandCoreControllerPodPrefix,
				operandCertControllerPodPrefix,
				operandWebhookPodPrefix,
			})).To(Succeed(), "operand pods should still be running after custom label lifecycle test")
		})

		It("should apply and remove custom labels to all managed resources", func() {
			testLabels := map[string]string{
				"mycompany.io/env": "staging",
			}

			existingCR := &operatorv1alpha1.ExternalSecretsConfig{}
			Expect(runtimeClient.Get(ctx, client.ObjectKey{Name: "cluster"}, existingCR)).To(Succeed(),
				"should get ExternalSecretsConfig to capture initial state")
			originalLabels := maps.Clone(existingCR.Spec.ControllerConfig.Labels)

			defer func() {
				By("Restoring original labels on ExternalSecretsConfig CR")
				err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					currentCR := &operatorv1alpha1.ExternalSecretsConfig{}
					if err := runtimeClient.Get(ctx, client.ObjectKey{Name: "cluster"}, currentCR); err != nil {
						return err
					}
					currentCR.Spec.ControllerConfig.Labels = originalLabels
					return runtimeClient.Update(ctx, currentCR)
				})
				Expect(err).NotTo(HaveOccurred(), "should restore labels on ExternalSecretsConfig")
			}()

			By("Updating ExternalSecretsConfig with custom labels")
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				currentCR := &operatorv1alpha1.ExternalSecretsConfig{}
				if err := runtimeClient.Get(ctx, client.ObjectKey{Name: "cluster"}, currentCR); err != nil {
					return err
				}
				merged := make(map[string]string)
				if originalLabels != nil {
					maps.Copy(merged, originalLabels)
				}
				maps.Copy(merged, testLabels)
				currentCR.Spec.ControllerConfig.Labels = merged
				return runtimeClient.Update(ctx, currentCR)
			})
			Expect(err).NotTo(HaveOccurred(), "should update ExternalSecretsConfig with custom labels")

			By("Waiting for external-secrets operand pods to be ready")
			Expect(utils.VerifyPodsReadyByPrefix(ctx, clientset, operandNamespace, []string{
				operandCoreControllerPodPrefix,
				operandCertControllerPodPrefix,
				operandWebhookPodPrefix,
			})).To(Succeed())

			for _, resourceType := range getResourceTypesToVerify() {
				By(fmt.Sprintf("Verifying custom labels are applied to %s resources", resourceType.name))
				Eventually(func(g Gomega) {
					objects, err := resourceType.listFunc(ctx, clientset, operandNamespace, g)
					g.Expect(err).NotTo(HaveOccurred(), "should list %s", resourceType.name)

					for _, obj := range objects {
						if !strings.HasPrefix(obj.GetName(), "external-secrets") {
							continue
						}
						for k, v := range testLabels {
							g.Expect(obj.GetLabels()).To(HaveKeyWithValue(k, v),
								"%s %s should have label %s=%s", resourceType.name, obj.GetName(), k, v)
						}
					}
				}, 2*time.Minute, 5*time.Second).Should(Succeed())
			}

			By("Removing custom labels from ExternalSecretsConfig CR")
			err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				currentCR := &operatorv1alpha1.ExternalSecretsConfig{}
				if err := runtimeClient.Get(ctx, client.ObjectKey{Name: "cluster"}, currentCR); err != nil {
					return err
				}
				for k := range testLabels {
					delete(currentCR.Spec.ControllerConfig.Labels, k)
				}
				return runtimeClient.Update(ctx, currentCR)
			})
			Expect(err).NotTo(HaveOccurred(), "should remove custom labels from ExternalSecretsConfig")

			for _, resourceType := range getResourceTypesToVerify() {
				By(fmt.Sprintf("Verifying custom labels are removed from %s resources", resourceType.name))
				Eventually(func(g Gomega) {
					objects, err := resourceType.listFunc(ctx, clientset, operandNamespace, g)
					g.Expect(err).NotTo(HaveOccurred(), "should list %s", resourceType.name)

					for _, obj := range objects {
						if !strings.HasPrefix(obj.GetName(), "external-secrets") {
							continue
						}
						for k := range testLabels {
							g.Expect(obj.GetLabels()).NotTo(HaveKey(k),
								"%s %s should NOT have label %s after removal", resourceType.name, obj.GetName(), k)
						}
					}
				}, 2*time.Minute, 5*time.Second).Should(Succeed())
			}
		})
	})

	Context("Managed Label Restoration", Label("Platform:Generic", "Feature:CustomLabels"), func() {
		const (
			managedLabelKey   = "app"
			managedLabelValue = "external-secrets"
		)

		AfterEach(func() {
			By("Verifying ExternalSecretsConfig is Ready and not Degraded after label restoration")
			Expect(utils.WaitForExternalSecretsConfigReady(ctx, dynamicClient, "cluster", 2*time.Minute)).To(Succeed(),
				"ExternalSecretsConfig should remain Ready after label tampering and restoration")

			By("Verifying operand pods are still ready after label restoration")
			Expect(utils.VerifyPodsReadyByPrefix(ctx, clientset, operandNamespace, []string{
				operandCoreControllerPodPrefix,
				operandCertControllerPodPrefix,
				operandWebhookPodPrefix,
			})).To(Succeed(), "operand pods should still be running after label restoration")
		})

		It("should restore the app=external-secrets label on a ServiceAccount after external removal", func() {
			saName := "external-secrets"

			By("Verifying ServiceAccount has the managed label initially")
			Eventually(func(g Gomega) {
				sa, err := clientset.CoreV1().ServiceAccounts(operandNamespace).Get(ctx, saName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(sa.Labels).To(HaveKeyWithValue(managedLabelKey, managedLabelValue))
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Removing the managed label from the ServiceAccount")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				sa, err := clientset.CoreV1().ServiceAccounts(operandNamespace).Get(ctx, saName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				delete(sa.Labels, managedLabelKey)
				_, err = clientset.CoreV1().ServiceAccounts(operandNamespace).Update(ctx, sa, metav1.UpdateOptions{})
				return err
			})).To(Succeed(), "should remove the managed label")

			By("Waiting for operator to restore the managed label")
			Eventually(func(g Gomega) {
				sa, err := clientset.CoreV1().ServiceAccounts(operandNamespace).Get(ctx, saName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(sa.Labels).To(HaveKeyWithValue(managedLabelKey, managedLabelValue),
					"operator should restore app=external-secrets on ServiceAccount %s", saName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should restore the app=external-secrets label on a Role after external removal", func() {
			roleName := "external-secrets-leaderelection"

			By("Verifying Role has the managed label initially")
			Eventually(func(g Gomega) {
				role, err := clientset.RbacV1().Roles(operandNamespace).Get(ctx, roleName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(role.Labels).To(HaveKeyWithValue(managedLabelKey, managedLabelValue))
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Removing the managed label from the Role")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				role, err := clientset.RbacV1().Roles(operandNamespace).Get(ctx, roleName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				delete(role.Labels, managedLabelKey)
				_, err = clientset.RbacV1().Roles(operandNamespace).Update(ctx, role, metav1.UpdateOptions{})
				return err
			})).To(Succeed(), "should remove the managed label")

			By("Waiting for operator to restore the managed label")
			Eventually(func(g Gomega) {
				role, err := clientset.RbacV1().Roles(operandNamespace).Get(ctx, roleName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(role.Labels).To(HaveKeyWithValue(managedLabelKey, managedLabelValue),
					"operator should restore app=external-secrets on Role %s", roleName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should restore the app=external-secrets label on a Deployment after external removal", func() {
			depName := "external-secrets"

			By("Verifying Deployment has the managed label initially")
			Eventually(func(g Gomega) {
				dep, err := clientset.AppsV1().Deployments(operandNamespace).Get(ctx, depName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(dep.Labels).To(HaveKeyWithValue(managedLabelKey, managedLabelValue))
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Removing the managed label from the Deployment")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				dep, err := clientset.AppsV1().Deployments(operandNamespace).Get(ctx, depName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				delete(dep.Labels, managedLabelKey)
				_, err = clientset.AppsV1().Deployments(operandNamespace).Update(ctx, dep, metav1.UpdateOptions{})
				return err
			})).To(Succeed(), "should remove the managed label")

			By("Waiting for operator to restore the managed label")
			Eventually(func(g Gomega) {
				dep, err := clientset.AppsV1().Deployments(operandNamespace).Get(ctx, depName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(dep.Labels).To(HaveKeyWithValue(managedLabelKey, managedLabelValue),
					"operator should restore app=external-secrets on Deployment %s", depName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		// Secret uses patchResourceMetadata (JSON Patch) rather than UpdateWithRetry because
		// its Data field is co-managed by the cert-controller. This test specifically exercises
		// that the metadata-only patch path correctly restores the managed label.
		It("should restore the app=external-secrets label on a Secret after external removal", func() {
			secretName := "external-secrets-webhook"

			By("Verifying Secret has the managed label initially")
			Eventually(func(g Gomega) {
				secret, err := clientset.CoreV1().Secrets(operandNamespace).Get(ctx, secretName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(secret.Labels).To(HaveKeyWithValue(managedLabelKey, managedLabelValue))
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Removing the managed label from the Secret")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				secret, err := clientset.CoreV1().Secrets(operandNamespace).Get(ctx, secretName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				delete(secret.Labels, managedLabelKey)
				_, err = clientset.CoreV1().Secrets(operandNamespace).Update(ctx, secret, metav1.UpdateOptions{})
				return err
			})).To(Succeed(), "should remove the managed label")

			By("Waiting for operator to restore the managed label")
			Eventually(func(g Gomega) {
				secret, err := clientset.CoreV1().Secrets(operandNamespace).Get(ctx, secretName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(secret.Labels).To(HaveKeyWithValue(managedLabelKey, managedLabelValue),
					"operator should restore app=external-secrets on Secret %s", secretName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should restore the app=external-secrets label on a Service after external removal", func() {
			serviceName := "external-secrets-webhook"

			By("Verifying Service has the managed label initially")
			Eventually(func(g Gomega) {
				svc, err := clientset.CoreV1().Services(operandNamespace).Get(ctx, serviceName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(svc.Labels).To(HaveKeyWithValue(managedLabelKey, managedLabelValue))
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Removing the managed label from the Service")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				svc, err := clientset.CoreV1().Services(operandNamespace).Get(ctx, serviceName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				delete(svc.Labels, managedLabelKey)
				_, err = clientset.CoreV1().Services(operandNamespace).Update(ctx, svc, metav1.UpdateOptions{})
				return err
			})).To(Succeed(), "should remove the managed label")

			By("Waiting for operator to restore the managed label")
			Eventually(func(g Gomega) {
				svc, err := clientset.CoreV1().Services(operandNamespace).Get(ctx, serviceName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(svc.Labels).To(HaveKeyWithValue(managedLabelKey, managedLabelValue),
					"operator should restore app=external-secrets on Service %s", serviceName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should restore the app=external-secrets label on a RoleBinding after external removal", func() {
			roleBindingName := "external-secrets-leaderelection"

			By("Verifying RoleBinding has the managed label initially")
			Eventually(func(g Gomega) {
				rb, err := clientset.RbacV1().RoleBindings(operandNamespace).Get(ctx, roleBindingName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(rb.Labels).To(HaveKeyWithValue(managedLabelKey, managedLabelValue))
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Removing the managed label from the RoleBinding")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				rb, err := clientset.RbacV1().RoleBindings(operandNamespace).Get(ctx, roleBindingName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				delete(rb.Labels, managedLabelKey)
				_, err = clientset.RbacV1().RoleBindings(operandNamespace).Update(ctx, rb, metav1.UpdateOptions{})
				return err
			})).To(Succeed(), "should remove the managed label")

			By("Waiting for operator to restore the managed label")
			Eventually(func(g Gomega) {
				rb, err := clientset.RbacV1().RoleBindings(operandNamespace).Get(ctx, roleBindingName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(rb.Labels).To(HaveKeyWithValue(managedLabelKey, managedLabelValue),
					"operator should restore app=external-secrets on RoleBinding %s", roleBindingName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should restore the app=external-secrets label on a ClusterRole after external removal", func() {
			clusterRoleName := "external-secrets-controller"

			By("Verifying ClusterRole has the managed label initially")
			Eventually(func(g Gomega) {
				cr, err := clientset.RbacV1().ClusterRoles().Get(ctx, clusterRoleName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Labels).To(HaveKeyWithValue(managedLabelKey, managedLabelValue))
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Removing the managed label from the ClusterRole")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				cr, err := clientset.RbacV1().ClusterRoles().Get(ctx, clusterRoleName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				delete(cr.Labels, managedLabelKey)
				_, err = clientset.RbacV1().ClusterRoles().Update(ctx, cr, metav1.UpdateOptions{})
				return err
			})).To(Succeed(), "should remove the managed label")

			By("Waiting for operator to restore the managed label")
			Eventually(func(g Gomega) {
				cr, err := clientset.RbacV1().ClusterRoles().Get(ctx, clusterRoleName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Labels).To(HaveKeyWithValue(managedLabelKey, managedLabelValue),
					"operator should restore app=external-secrets on ClusterRole %s", clusterRoleName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should restore the app=external-secrets label on a ClusterRoleBinding after external removal", func() {
			clusterRoleBindingName := "external-secrets-controller"

			By("Verifying ClusterRoleBinding has the managed label initially")
			Eventually(func(g Gomega) {
				crb, err := clientset.RbacV1().ClusterRoleBindings().Get(ctx, clusterRoleBindingName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(crb.Labels).To(HaveKeyWithValue(managedLabelKey, managedLabelValue))
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Removing the managed label from the ClusterRoleBinding")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				crb, err := clientset.RbacV1().ClusterRoleBindings().Get(ctx, clusterRoleBindingName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				delete(crb.Labels, managedLabelKey)
				_, err = clientset.RbacV1().ClusterRoleBindings().Update(ctx, crb, metav1.UpdateOptions{})
				return err
			})).To(Succeed(), "should remove the managed label")

			By("Waiting for operator to restore the managed label")
			Eventually(func(g Gomega) {
				crb, err := clientset.RbacV1().ClusterRoleBindings().Get(ctx, clusterRoleBindingName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(crb.Labels).To(HaveKeyWithValue(managedLabelKey, managedLabelValue),
					"operator should restore app=external-secrets on ClusterRoleBinding %s", clusterRoleBindingName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should restore the app=external-secrets label on a NetworkPolicy after external removal", func() {
			npName := "eso-sys-deny-all-traffic"

			By("Verifying NetworkPolicy has the managed label initially")
			Eventually(func(g Gomega) {
				np, err := clientset.NetworkingV1().NetworkPolicies(operandNamespace).Get(ctx, npName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(np.Labels).To(HaveKeyWithValue(managedLabelKey, managedLabelValue))
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("Removing the managed label from the NetworkPolicy")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				np, err := clientset.NetworkingV1().NetworkPolicies(operandNamespace).Get(ctx, npName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				delete(np.Labels, managedLabelKey)
				_, err = clientset.NetworkingV1().NetworkPolicies(operandNamespace).Update(ctx, np, metav1.UpdateOptions{})
				return err
			})).To(Succeed(), "should remove the managed label")

			By("Waiting for operator to restore the managed label")
			Eventually(func(g Gomega) {
				np, err := clientset.NetworkingV1().NetworkPolicies(operandNamespace).Get(ctx, npName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(np.Labels).To(HaveKeyWithValue(managedLabelKey, managedLabelValue),
					"operator should restore app=external-secrets on NetworkPolicy %s", npName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

	})

	AfterAll(func() {
		By("Deleting the externalsecrets.openshift.operator.io/cluster CR")
		loader.DeleteFromFile(testassets.ReadFile, externalSecretsFile, "")

		By("Deleting the test namespace")
		Expect(clientset.CoreV1().Namespaces().Delete(ctx, testNamespace, metav1.DeleteOptions{})).
			NotTo(HaveOccurred(), "failed to delete test namespace")
	})
})

// expectedProxyPorts derives the deduplicated TCP ports the operator should use for the
// proxy egress NetworkPolicy, mirroring the logic in extractProxyPorts. It collects ports
// from both httpsProxy and httpProxy URLs. An explicit port in the URL wins; otherwise
// scheme defaults apply (443 for https, 80 for http).
func expectedProxyPorts(httpsProxy, httpProxy string) []int32 {
	seen := map[int32]struct{}{}
	var ports []int32
	for _, raw := range []string{httpsProxy, httpProxy} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		var port int32
		if p := u.Port(); p != "" {
			if v, err := strconv.Atoi(p); err == nil && v > 0 {
				port = int32(v)
			}
		}
		if port == 0 {
			switch strings.ToLower(u.Scheme) {
			case "https":
				port = 443
			case "http":
				port = 80
			}
		}
		if port > 0 {
			if _, exists := seen[port]; !exists {
				seen[port] = struct{}{}
				ports = append(ports, port)
			}
		}
	}
	return ports
}
