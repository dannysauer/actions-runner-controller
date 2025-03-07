package actionsgithubcom

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	actionsv1alpha1 "github.com/actions/actions-runner-controller/apis/actions.github.com/v1alpha1"
	v1alpha1 "github.com/actions/actions-runner-controller/apis/actions.github.com/v1alpha1"
	"github.com/actions/actions-runner-controller/github/actions"
	"github.com/actions/actions-runner-controller/github/actions/fake"
)

const (
	ephemeralRunnerSetTestTimeout     = time.Second * 10
	ephemeralRunnerSetTestInterval    = time.Millisecond * 250
	ephemeralRunnerSetTestGitHubToken = "gh_token"
)

var _ = Describe("Test EphemeralRunnerSet controller", func() {
	var ctx context.Context
	var mgr ctrl.Manager
	var autoscalingNS *corev1.Namespace
	var ephemeralRunnerSet *actionsv1alpha1.EphemeralRunnerSet
	var configSecret *corev1.Secret

	BeforeEach(func() {
		ctx = context.Background()
		autoscalingNS, mgr = createNamespace(GinkgoT(), k8sClient)
		configSecret = createDefaultSecret(GinkgoT(), k8sClient, autoscalingNS.Name)

		controller := &EphemeralRunnerSetReconciler{
			Client:        mgr.GetClient(),
			Scheme:        mgr.GetScheme(),
			Log:           logf.Log,
			ActionsClient: fake.NewMultiClient(),
		}
		err := controller.SetupWithManager(mgr)
		Expect(err).NotTo(HaveOccurred(), "failed to setup controller")

		ephemeralRunnerSet = &actionsv1alpha1.EphemeralRunnerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-asrs",
				Namespace: autoscalingNS.Name,
			},
			Spec: actionsv1alpha1.EphemeralRunnerSetSpec{
				EphemeralRunnerSpec: actionsv1alpha1.EphemeralRunnerSpec{
					GitHubConfigUrl:    "https://github.com/owner/repo",
					GitHubConfigSecret: configSecret.Name,
					RunnerScaleSetId:   100,
					PodTemplateSpec: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "runner",
									Image: "ghcr.io/actions/runner",
								},
							},
						},
					},
				},
			},
		}

		err = k8sClient.Create(ctx, ephemeralRunnerSet)
		Expect(err).NotTo(HaveOccurred(), "failed to create EphemeralRunnerSet")

		startManagers(GinkgoT(), mgr)
	})

	Context("When creating a new EphemeralRunnerSet", func() {
		It("It should create/add all required resources for a new EphemeralRunnerSet (finalizer)", func() {
			// Check if finalizer is added
			created := new(actionsv1alpha1.EphemeralRunnerSet)
			Eventually(
				func() (string, error) {
					err := k8sClient.Get(ctx, client.ObjectKey{Name: ephemeralRunnerSet.Name, Namespace: ephemeralRunnerSet.Namespace}, created)
					if err != nil {
						return "", err
					}
					if len(created.Finalizers) == 0 {
						return "", nil
					}
					return created.Finalizers[0], nil
				},
				ephemeralRunnerSetTestTimeout,
				ephemeralRunnerSetTestInterval).Should(BeEquivalentTo(ephemeralRunnerSetFinalizerName), "EphemeralRunnerSet should have a finalizer")

			// Check if the number of ephemeral runners are stay 0
			Consistently(
				func() (int, error) {
					runnerList := new(actionsv1alpha1.EphemeralRunnerList)
					err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
					if err != nil {
						return -1, err
					}

					return len(runnerList.Items), nil
				},
				ephemeralRunnerSetTestTimeout,
				ephemeralRunnerSetTestInterval).Should(BeEquivalentTo(0), "No EphemeralRunner should be created")

			// Check if the status stay 0
			Consistently(
				func() (int, error) {
					runnerSet := new(actionsv1alpha1.EphemeralRunnerSet)
					err := k8sClient.Get(ctx, client.ObjectKey{Name: ephemeralRunnerSet.Name, Namespace: ephemeralRunnerSet.Namespace}, runnerSet)
					if err != nil {
						return -1, err
					}

					return int(runnerSet.Status.CurrentReplicas), nil
				},
				ephemeralRunnerSetTestTimeout,
				ephemeralRunnerSetTestInterval).Should(BeEquivalentTo(0), "EphemeralRunnerSet status should be 0")

			// Scaling up the EphemeralRunnerSet
			updated := created.DeepCopy()
			updated.Spec.Replicas = 5
			err := k8sClient.Update(ctx, updated)
			Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunnerSet")

			// Check if the number of ephemeral runners are created
			Eventually(
				func() (int, error) {
					runnerList := new(actionsv1alpha1.EphemeralRunnerList)
					err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
					if err != nil {
						return -1, err
					}

					// Set status to simulate a configured EphemeralRunner
					refetch := false
					for i, runner := range runnerList.Items {
						if runner.Status.RunnerId == 0 {
							updatedRunner := runner.DeepCopy()
							updatedRunner.Status.Phase = corev1.PodRunning
							updatedRunner.Status.RunnerId = i + 100
							err = k8sClient.Status().Patch(ctx, updatedRunner, client.MergeFrom(&runner))
							Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunner")
							refetch = true
						}
					}

					if refetch {
						err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
						if err != nil {
							return -1, err
						}
					}

					return len(runnerList.Items), nil
				},
				ephemeralRunnerSetTestTimeout,
				ephemeralRunnerSetTestInterval).Should(BeEquivalentTo(5), "5 EphemeralRunner should be created")

			// Check if the status is updated
			Eventually(
				func() (int, error) {
					runnerSet := new(actionsv1alpha1.EphemeralRunnerSet)
					err := k8sClient.Get(ctx, client.ObjectKey{Name: ephemeralRunnerSet.Name, Namespace: ephemeralRunnerSet.Namespace}, runnerSet)
					if err != nil {
						return -1, err
					}

					return int(runnerSet.Status.CurrentReplicas), nil
				},
				ephemeralRunnerSetTestTimeout,
				ephemeralRunnerSetTestInterval).Should(BeEquivalentTo(5), "EphemeralRunnerSet status should be 5")
		})
	})

	Context("When deleting a new EphemeralRunnerSet", func() {
		It("It should cleanup all resources for a deleting EphemeralRunnerSet before removing it", func() {
			created := new(actionsv1alpha1.EphemeralRunnerSet)
			err := k8sClient.Get(ctx, client.ObjectKey{Name: ephemeralRunnerSet.Name, Namespace: ephemeralRunnerSet.Namespace}, created)
			Expect(err).NotTo(HaveOccurred(), "failed to get EphemeralRunnerSet")

			// Scale up the EphemeralRunnerSet
			updated := created.DeepCopy()
			updated.Spec.Replicas = 5
			err = k8sClient.Update(ctx, updated)
			Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunnerSet")

			// Wait for the EphemeralRunnerSet to be scaled up
			Eventually(
				func() (int, error) {
					runnerList := new(actionsv1alpha1.EphemeralRunnerList)
					err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
					if err != nil {
						return -1, err
					}

					// Set status to simulate a configured EphemeralRunner
					refetch := false
					for i, runner := range runnerList.Items {
						if runner.Status.RunnerId == 0 {
							updatedRunner := runner.DeepCopy()
							updatedRunner.Status.Phase = corev1.PodRunning
							updatedRunner.Status.RunnerId = i + 100
							err = k8sClient.Status().Patch(ctx, updatedRunner, client.MergeFrom(&runner))
							Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunner")
							refetch = true
						}
					}

					if refetch {
						err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
						if err != nil {
							return -1, err
						}
					}

					return len(runnerList.Items), nil
				},
				ephemeralRunnerSetTestTimeout,
				ephemeralRunnerSetTestInterval).Should(BeEquivalentTo(5), "5 EphemeralRunner should be created")

			// Delete the EphemeralRunnerSet
			err = k8sClient.Delete(ctx, created)
			Expect(err).NotTo(HaveOccurred(), "failed to delete EphemeralRunnerSet")

			// Check if all ephemeral runners are deleted
			Eventually(
				func() (int, error) {
					runnerList := new(actionsv1alpha1.EphemeralRunnerList)
					err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
					if err != nil {
						return -1, err
					}

					return len(runnerList.Items), nil
				},
				ephemeralRunnerSetTestTimeout,
				ephemeralRunnerSetTestInterval).Should(BeEquivalentTo(0), "All EphemeralRunner should be deleted")

			// Check if the EphemeralRunnerSet is deleted
			Eventually(
				func() error {
					deleted := new(actionsv1alpha1.EphemeralRunnerSet)
					err = k8sClient.Get(ctx, client.ObjectKey{Name: ephemeralRunnerSet.Name, Namespace: ephemeralRunnerSet.Namespace}, deleted)
					if err != nil {
						if kerrors.IsNotFound(err) {
							return nil
						}

						return err
					}

					return fmt.Errorf("EphemeralRunnerSet is not deleted")
				},
				ephemeralRunnerSetTestTimeout,
				ephemeralRunnerSetTestInterval).Should(Succeed(), "EphemeralRunnerSet should be deleted")
		})
	})

	Context("When a new EphemeralRunnerSet scale up and down", func() {
		It("It should delete finished EphemeralRunner and create new EphemeralRunner", func() {
			created := new(actionsv1alpha1.EphemeralRunnerSet)
			err := k8sClient.Get(ctx, client.ObjectKey{Name: ephemeralRunnerSet.Name, Namespace: ephemeralRunnerSet.Namespace}, created)
			Expect(err).NotTo(HaveOccurred(), "failed to get EphemeralRunnerSet")

			// Scale up the EphemeralRunnerSet
			updated := created.DeepCopy()
			updated.Spec.Replicas = 5
			err = k8sClient.Update(ctx, updated)
			Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunnerSet")

			// Wait for the EphemeralRunnerSet to be scaled up
			runnerList := new(actionsv1alpha1.EphemeralRunnerList)
			Eventually(
				func() (int, error) {
					err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
					if err != nil {
						return -1, err
					}

					// Set status to simulate a configured EphemeralRunner
					refetch := false
					for i, runner := range runnerList.Items {
						if runner.Status.RunnerId == 0 {
							updatedRunner := runner.DeepCopy()
							updatedRunner.Status.Phase = corev1.PodRunning
							updatedRunner.Status.RunnerId = i + 100
							err = k8sClient.Status().Patch(ctx, updatedRunner, client.MergeFrom(&runner))
							Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunner")
							refetch = true
						}
					}

					if refetch {
						err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
						if err != nil {
							return -1, err
						}
					}

					return len(runnerList.Items), nil
				},
				ephemeralRunnerSetTestTimeout,
				ephemeralRunnerSetTestInterval).Should(BeEquivalentTo(5), "5 EphemeralRunner should be created")

			// Mark one of the EphemeralRunner as finished
			finishedRunner := runnerList.Items[4].DeepCopy()
			finishedRunner.Status.Phase = corev1.PodSucceeded
			err = k8sClient.Status().Patch(ctx, finishedRunner, client.MergeFrom(&runnerList.Items[4]))
			Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunner")

			// Wait for the finished EphemeralRunner to be deleted
			Eventually(
				func() error {
					runnerList := new(actionsv1alpha1.EphemeralRunnerList)
					err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
					if err != nil {
						return err
					}

					for _, runner := range runnerList.Items {
						if runner.Name == finishedRunner.Name {
							return fmt.Errorf("EphemeralRunner is not deleted")
						}
					}

					return nil
				},
				ephemeralRunnerSetTestTimeout,
				ephemeralRunnerSetTestInterval).Should(Succeed(), "Finished EphemeralRunner should be deleted")

			// We should still have the EphemeralRunnerSet scale up
			runnerList = new(actionsv1alpha1.EphemeralRunnerList)
			Eventually(
				func() (int, error) {
					err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
					if err != nil {
						return -1, err
					}

					// Set status to simulate a configured EphemeralRunner
					refetch := false
					for i, runner := range runnerList.Items {
						if runner.Status.RunnerId == 0 {
							updatedRunner := runner.DeepCopy()
							updatedRunner.Status.Phase = corev1.PodRunning
							updatedRunner.Status.RunnerId = i + 100
							err = k8sClient.Status().Patch(ctx, updatedRunner, client.MergeFrom(&runner))
							Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunner")
							refetch = true
						}
					}

					if refetch {
						err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
						if err != nil {
							return -1, err
						}
					}

					return len(runnerList.Items), nil
				},
				ephemeralRunnerSetTestTimeout,
				ephemeralRunnerSetTestInterval).Should(BeEquivalentTo(5), "5 EphemeralRunner should be created")

			// Scale down the EphemeralRunnerSet
			updated = created.DeepCopy()
			updated.Spec.Replicas = 3
			err = k8sClient.Patch(ctx, updated, client.MergeFrom(created))
			Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunnerSet")

			// Wait for the EphemeralRunnerSet to be scaled down
			runnerList = new(actionsv1alpha1.EphemeralRunnerList)
			Eventually(
				func() (int, error) {
					err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
					if err != nil {
						return -1, err
					}

					// Set status to simulate a configured EphemeralRunner
					refetch := false
					for i, runner := range runnerList.Items {
						if runner.Status.RunnerId == 0 {
							updatedRunner := runner.DeepCopy()
							updatedRunner.Status.Phase = corev1.PodRunning
							updatedRunner.Status.RunnerId = i + 100
							err = k8sClient.Status().Patch(ctx, updatedRunner, client.MergeFrom(&runner))
							Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunner")
							refetch = true
						}
					}

					if refetch {
						err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
						if err != nil {
							return -1, err
						}
					}

					return len(runnerList.Items), nil
				},
				ephemeralRunnerSetTestTimeout,
				ephemeralRunnerSetTestInterval).Should(BeEquivalentTo(3), "3 EphemeralRunner should be created")

			// We will not scale down runner that is running jobs
			runningRunner := runnerList.Items[0].DeepCopy()
			runningRunner.Status.JobRequestId = 1000
			err = k8sClient.Status().Patch(ctx, runningRunner, client.MergeFrom(&runnerList.Items[0]))
			Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunner")

			runningRunner = runnerList.Items[1].DeepCopy()
			runningRunner.Status.JobRequestId = 1001
			err = k8sClient.Status().Patch(ctx, runningRunner, client.MergeFrom(&runnerList.Items[1]))
			Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunner")

			// Scale down to 1
			updated = created.DeepCopy()
			updated.Spec.Replicas = 1
			err = k8sClient.Patch(ctx, updated, client.MergeFrom(created))
			Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunnerSet")

			// Wait for the EphemeralRunnerSet to be scaled down to 2 since we still have 2 runner running jobs
			runnerList = new(actionsv1alpha1.EphemeralRunnerList)
			Eventually(
				func() (int, error) {
					err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
					if err != nil {
						return -1, err
					}

					// Set status to simulate a configured EphemeralRunner
					refetch := false
					for i, runner := range runnerList.Items {
						if runner.Status.RunnerId == 0 {
							updatedRunner := runner.DeepCopy()
							updatedRunner.Status.Phase = corev1.PodRunning
							updatedRunner.Status.RunnerId = i + 100
							err = k8sClient.Status().Patch(ctx, updatedRunner, client.MergeFrom(&runner))
							Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunner")
							refetch = true
						}
					}

					if refetch {
						err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
						if err != nil {
							return -1, err
						}
					}

					return len(runnerList.Items), nil
				},
				ephemeralRunnerSetTestTimeout,
				ephemeralRunnerSetTestInterval).Should(BeEquivalentTo(2), "2 EphemeralRunner should be created")

			// We will not scale down failed runner
			failedRunner := runnerList.Items[0].DeepCopy()
			failedRunner.Status.Phase = corev1.PodFailed
			err = k8sClient.Status().Patch(ctx, failedRunner, client.MergeFrom(&runnerList.Items[0]))
			Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunner")

			// Scale down to 0
			updated = created.DeepCopy()
			updated.Spec.Replicas = 0
			err = k8sClient.Patch(ctx, updated, client.MergeFrom(created))
			Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunnerSet")

			// We should not scale down the EphemeralRunnerSet since we still have 1 runner running job and 1 failed runner
			runnerList = new(actionsv1alpha1.EphemeralRunnerList)
			Consistently(
				func() (int, error) {
					err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
					if err != nil {
						return -1, err
					}

					// Set status to simulate a configured EphemeralRunner
					refetch := false
					for i, runner := range runnerList.Items {
						if runner.Status.RunnerId == 0 {
							updatedRunner := runner.DeepCopy()
							updatedRunner.Status.Phase = corev1.PodRunning
							updatedRunner.Status.RunnerId = i + 100
							err = k8sClient.Status().Patch(ctx, updatedRunner, client.MergeFrom(&runner))
							Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunner")
							refetch = true
						}
					}

					if refetch {
						err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
						if err != nil {
							return -1, err
						}
					}

					return len(runnerList.Items), nil
				},
				ephemeralRunnerSetTestTimeout,
				ephemeralRunnerSetTestInterval).Should(BeEquivalentTo(2), "2 EphemeralRunner should be created")

			// We will scale down to 0 when the running job is completed and the failed runner is deleted
			runningRunner = runnerList.Items[1].DeepCopy()
			runningRunner.Status.Phase = corev1.PodSucceeded
			err = k8sClient.Status().Patch(ctx, runningRunner, client.MergeFrom(&runnerList.Items[1]))
			Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunner")

			err = k8sClient.Delete(ctx, &runnerList.Items[0])
			Expect(err).NotTo(HaveOccurred(), "failed to delete EphemeralRunner")

			// Wait for the EphemeralRunnerSet to be scaled down to 0
			runnerList = new(actionsv1alpha1.EphemeralRunnerList)
			Eventually(
				func() (int, error) {
					err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
					if err != nil {
						return -1, err
					}

					// Set status to simulate a configured EphemeralRunner
					refetch := false
					for i, runner := range runnerList.Items {
						if runner.Status.RunnerId == 0 {
							updatedRunner := runner.DeepCopy()
							updatedRunner.Status.Phase = corev1.PodRunning
							updatedRunner.Status.RunnerId = i + 100
							err = k8sClient.Status().Patch(ctx, updatedRunner, client.MergeFrom(&runner))
							Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunner")
							refetch = true
						}
					}

					if refetch {
						err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
						if err != nil {
							return -1, err
						}
					}

					return len(runnerList.Items), nil
				},
				ephemeralRunnerSetTestTimeout,
				ephemeralRunnerSetTestInterval).Should(BeEquivalentTo(0), "0 EphemeralRunner should be created")
		})
	})
})

var _ = Describe("Test EphemeralRunnerSet controller with proxy settings", func() {
	var ctx context.Context
	var mgr ctrl.Manager
	var autoscalingNS *corev1.Namespace
	var ephemeralRunnerSet *actionsv1alpha1.EphemeralRunnerSet
	var configSecret *corev1.Secret

	BeforeEach(func() {
		ctx = context.Background()
		autoscalingNS, mgr = createNamespace(GinkgoT(), k8sClient)
		configSecret = createDefaultSecret(GinkgoT(), k8sClient, autoscalingNS.Name)

		controller := &EphemeralRunnerSetReconciler{
			Client:        mgr.GetClient(),
			Scheme:        mgr.GetScheme(),
			Log:           logf.Log,
			ActionsClient: actions.NewMultiClient("test", logr.Discard()),
		}
		err := controller.SetupWithManager(mgr)
		Expect(err).NotTo(HaveOccurred(), "failed to setup controller")

		startManagers(GinkgoT(), mgr)
	})

	It("should create a proxy secret and delete the proxy secreat after the runner-set is deleted", func() {
		secretCredentials := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "proxy-credentials",
				Namespace: autoscalingNS.Name,
			},
			Data: map[string][]byte{
				"username": []byte("username"),
				"password": []byte("password"),
			},
		}

		err := k8sClient.Create(ctx, secretCredentials)
		Expect(err).NotTo(HaveOccurred(), "failed to create secret credentials")

		ephemeralRunnerSet = &actionsv1alpha1.EphemeralRunnerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-asrs",
				Namespace: autoscalingNS.Name,
			},
			Spec: actionsv1alpha1.EphemeralRunnerSetSpec{
				Replicas: 1,
				EphemeralRunnerSpec: actionsv1alpha1.EphemeralRunnerSpec{
					GitHubConfigUrl:    "http://example.com/owner/repo",
					GitHubConfigSecret: configSecret.Name,
					RunnerScaleSetId:   100,
					Proxy: &v1alpha1.ProxyConfig{
						HTTP: &v1alpha1.ProxyServerConfig{
							Url:                 "http://proxy.example.com",
							CredentialSecretRef: secretCredentials.Name,
						},
						HTTPS: &v1alpha1.ProxyServerConfig{
							Url:                 "https://proxy.example.com",
							CredentialSecretRef: secretCredentials.Name,
						},
						NoProxy: []string{"example.com", "example.org"},
					},
					PodTemplateSpec: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "runner",
									Image: "ghcr.io/actions/runner",
								},
							},
						},
					},
				},
			},
		}

		err = k8sClient.Create(ctx, ephemeralRunnerSet)
		Expect(err).NotTo(HaveOccurred(), "failed to create EphemeralRunnerSet")

		Eventually(func(g Gomega) {
			// Compiled / flattened proxy secret should exist at this point
			actualProxySecret := &corev1.Secret{}
			err = k8sClient.Get(ctx, client.ObjectKey{
				Namespace: autoscalingNS.Name,
				Name:      proxyEphemeralRunnerSetSecretName(ephemeralRunnerSet),
			}, actualProxySecret)
			g.Expect(err).NotTo(HaveOccurred(), "failed to get compiled / flattened proxy secret")

			secretFetcher := func(name string) (*corev1.Secret, error) {
				secret := &corev1.Secret{}
				err = k8sClient.Get(ctx, client.ObjectKey{
					Namespace: autoscalingNS.Name,
					Name:      name,
				}, secret)
				return secret, err
			}

			// Assert that the proxy secret is created with the correct values
			expectedData, err := ephemeralRunnerSet.Spec.EphemeralRunnerSpec.Proxy.ToSecretData(secretFetcher)
			g.Expect(err).NotTo(HaveOccurred(), "failed to get proxy secret data")
			g.Expect(actualProxySecret.Data).To(Equal(expectedData))
		},
			ephemeralRunnerSetTestTimeout,
			ephemeralRunnerSetTestInterval,
		).Should(Succeed(), "compiled / flattened proxy secret should exist")

		Eventually(func(g Gomega) {
			runnerList := new(actionsv1alpha1.EphemeralRunnerList)
			err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
			g.Expect(err).NotTo(HaveOccurred(), "failed to list EphemeralRunners")

			for _, runner := range runnerList.Items {
				g.Expect(runner.Spec.ProxySecretRef).To(Equal(proxyEphemeralRunnerSetSecretName(ephemeralRunnerSet)))
			}
		}, ephemeralRunnerSetTestTimeout, ephemeralRunnerSetTestInterval).Should(Succeed(), "EphemeralRunners should have a reference to the proxy secret")

		// patch ephemeral runner set to have 0 replicas
		patch := client.MergeFrom(ephemeralRunnerSet.DeepCopy())
		ephemeralRunnerSet.Spec.Replicas = 0
		err = k8sClient.Patch(ctx, ephemeralRunnerSet, patch)
		Expect(err).NotTo(HaveOccurred(), "failed to patch EphemeralRunnerSet")

		// Set pods to PodSucceeded to simulate an actual EphemeralRunner stopping
		Eventually(
			func(g Gomega) (int, error) {
				runnerList := new(actionsv1alpha1.EphemeralRunnerList)
				err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
				if err != nil {
					return -1, err
				}

				// Set status to simulate a configured EphemeralRunner
				refetch := false
				for i, runner := range runnerList.Items {
					if runner.Status.RunnerId == 0 {
						updatedRunner := runner.DeepCopy()
						updatedRunner.Status.Phase = corev1.PodSucceeded
						updatedRunner.Status.RunnerId = i + 100
						err = k8sClient.Status().Patch(ctx, updatedRunner, client.MergeFrom(&runner))
						Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunner")
						refetch = true
					}
				}

				if refetch {
					err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
					if err != nil {
						return -1, err
					}
				}

				return len(runnerList.Items), nil
			},
			ephemeralRunnerSetTestTimeout,
			ephemeralRunnerSetTestInterval).Should(BeEquivalentTo(1), "1 EphemeralRunner should exist")

		// Delete the EphemeralRunnerSet
		err = k8sClient.Delete(ctx, ephemeralRunnerSet)
		Expect(err).NotTo(HaveOccurred(), "failed to delete EphemeralRunnerSet")

		// Assert that the proxy secret is deleted
		Eventually(func(g Gomega) {
			proxySecret := &corev1.Secret{}
			err = k8sClient.Get(ctx, client.ObjectKey{
				Namespace: autoscalingNS.Name,
				Name:      proxyEphemeralRunnerSetSecretName(ephemeralRunnerSet),
			}, proxySecret)
			g.Expect(err).To(HaveOccurred(), "proxy secret should be deleted")
			g.Expect(kerrors.IsNotFound(err)).To(BeTrue(), "proxy secret should be deleted")
		},
			ephemeralRunnerSetTestTimeout,
			ephemeralRunnerSetTestInterval,
		).Should(Succeed(), "proxy secret should be deleted")
	})

	It("should configure the actions client to use proxy details", func() {
		secretCredentials := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "proxy-credentials",
				Namespace: autoscalingNS.Name,
			},
			Data: map[string][]byte{
				"username": []byte("test"),
				"password": []byte("password"),
			},
		}

		err := k8sClient.Create(ctx, secretCredentials)
		Expect(err).NotTo(HaveOccurred(), "failed to create secret credentials")

		proxySuccessfulllyCalled := false
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Proxy-Authorization")
			Expect(header).NotTo(BeEmpty())

			header = strings.TrimPrefix(header, "Basic ")
			decoded, err := base64.StdEncoding.DecodeString(header)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(decoded)).To(Equal("test:password"))

			proxySuccessfulllyCalled = true
			w.WriteHeader(http.StatusOK)
		}))
		GinkgoT().Cleanup(func() {
			proxy.Close()
		})

		ephemeralRunnerSet = &actionsv1alpha1.EphemeralRunnerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-asrs",
				Namespace: autoscalingNS.Name,
			},
			Spec: actionsv1alpha1.EphemeralRunnerSetSpec{
				Replicas: 1,
				EphemeralRunnerSpec: actionsv1alpha1.EphemeralRunnerSpec{
					GitHubConfigUrl:    "http://example.com/owner/repo",
					GitHubConfigSecret: configSecret.Name,
					RunnerScaleSetId:   100,
					Proxy: &v1alpha1.ProxyConfig{
						HTTP: &v1alpha1.ProxyServerConfig{
							Url:                 proxy.URL,
							CredentialSecretRef: "proxy-credentials",
						},
					},
					PodTemplateSpec: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "runner",
									Image: "ghcr.io/actions/runner",
								},
							},
						},
					},
				},
			},
		}

		err = k8sClient.Create(ctx, ephemeralRunnerSet)
		Expect(err).NotTo(HaveOccurred(), "failed to create EphemeralRunnerSet")

		runnerList := new(actionsv1alpha1.EphemeralRunnerList)
		Eventually(func() (int, error) {
			err := k8sClient.List(ctx, runnerList, client.InNamespace(ephemeralRunnerSet.Namespace))
			if err != nil {
				return -1, err
			}

			return len(runnerList.Items), nil
		},
			ephemeralRunnerSetTestTimeout,
			ephemeralRunnerSetTestInterval,
		).Should(BeEquivalentTo(1), "failed to create ephemeral runner")

		runner := runnerList.Items[0].DeepCopy()
		runner.Status.Phase = corev1.PodRunning
		runner.Status.RunnerId = 100
		err = k8sClient.Status().Patch(ctx, runner, client.MergeFrom(&runnerList.Items[0]))
		Expect(err).NotTo(HaveOccurred(), "failed to update ephemeral runner status")

		updatedRunnerSet := new(actionsv1alpha1.EphemeralRunnerSet)
		err = k8sClient.Get(ctx, client.ObjectKey{Namespace: ephemeralRunnerSet.Namespace, Name: ephemeralRunnerSet.Name}, updatedRunnerSet)
		Expect(err).NotTo(HaveOccurred(), "failed to get EphemeralRunnerSet")

		updatedRunnerSet.Spec.Replicas = 0
		err = k8sClient.Update(ctx, updatedRunnerSet)
		Expect(err).NotTo(HaveOccurred(), "failed to update EphemeralRunnerSet")

		Eventually(
			func() bool {
				return proxySuccessfulllyCalled
			},
			2*time.Second,
			interval,
		).Should(BeEquivalentTo(true))
	})
})
