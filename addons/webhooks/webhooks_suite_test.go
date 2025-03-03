// Copyright 2022 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package webhooks_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	csiv1alpha1 "github.com/vmware-tanzu/tanzu-framework/apis/addonconfigs/csi/v1alpha1"

	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"

	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterapiv1beta1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	kapppkgv1alpha1 "github.com/vmware-tanzu/carvel-kapp-controller/pkg/apiserver/apis/datapackaging/v1alpha1"
	"github.com/vmware-tanzu/tanzu-framework/addons/test/testutil"
	addonwebhooks "github.com/vmware-tanzu/tanzu-framework/addons/webhooks"
	runtanzuv1alpha3 "github.com/vmware-tanzu/tanzu-framework/apis/run/v1alpha3"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const (
	SystemNamespace                     = "tkg-system"
	clusterBootstrapWebhookManifestFile = "clusterbootstrap-webhook-manifests.yaml"
)

var (
	k8sClient     client.Client
	testEnv       *envtest.Environment
	ctx           = ctrl.SetupSignalHandler()
	scheme        = runtime.NewScheme()
	mgr           manager.Manager
	dynamicClient dynamic.Interface
	cancel        context.CancelFunc
)

func TestWebhooks(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Webhooks Suite")
}

var _ = BeforeSuite(func(done Done) {

	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	By("bootstrapping test environment")
	tmpDir, err := os.MkdirTemp("/tmp", "webhooktest")
	Expect(err).ToNot(HaveOccurred())
	externalDeps := map[string][]string{
		"sigs.k8s.io/cluster-api/api/v1beta1": {"config/crd/bases",
			"controlplane/kubeadm/config/crd/bases"},
	}
	externalCRDPaths, err := testutil.GetExternalCRDPaths(externalDeps)
	Expect(err).NotTo(HaveOccurred())
	Expect(externalCRDPaths).ToNot(BeEmpty())

	testEnv = &envtest.Environment{
		CRDInstallOptions:     envtest.CRDInstallOptions{CleanUpAfterUse: true},
		ErrorIfCRDPathMissing: true,
		CRDDirectoryPaths:     append(externalCRDPaths, constructCRDPaths()...),
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			LocalServingHost:    "127.0.0.1",
			LocalServingPort:    9447, // 9443 has been used by clusterbootstrap_test, using 9447 to avoid conflicts
			LocalServingCertDir: tmpDir,
			Paths:               []string{filepath.Join("..", "controllers", "testdata", "webhooks", clusterBootstrapWebhookManifestFile)},
		},
	}

	cfg, err := testEnv.Start()
	Expect(err).ToNot(HaveOccurred())
	Expect(cfg).ToNot(BeNil())

	err = csiv1alpha1.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	err = apiregistrationv1.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	err = admissionv1beta1.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	err = runtanzuv1alpha3.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	err = kapppkgv1alpha1.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	err = clientgoscheme.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	Expect(err).ToNot(HaveOccurred())
	Expect(k8sClient).ToNot(BeNil())

	dynamicClient, err = dynamic.NewForConfig(cfg)
	Expect(err).ToNot(HaveOccurred())
	Expect(dynamicClient).ToNot(BeNil())

	err = clusterapiv1beta1.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	webhookInstallOptions := &testEnv.WebhookInstallOptions
	options := manager.Options{
		Scheme:             scheme,
		MetricsBindAddress: "0",
		Host:               webhookInstallOptions.LocalServingHost,
		Port:               webhookInstallOptions.LocalServingPort,
		CertDir:            webhookInstallOptions.LocalServingCertDir,
		LeaderElection:     false,
	}
	mgr, err = ctrl.NewManager(testEnv.Config, options)
	Expect(err).ToNot(HaveOccurred())

	// pre-create namespace
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tkg-system"}}
	Expect(k8sClient.Create(context.TODO(), ns)).To(Succeed())

	// Set up the webhooks in the manager
	clusterBootstrapWebhook := addonwebhooks.ClusterBootstrap{
		Client:          k8sClient,
		SystemNamespace: SystemNamespace,
	}
	ctx, cancel = context.WithCancel(ctx)
	err = clusterBootstrapWebhook.SetupWebhookWithManager(ctx, mgr)
	Expect(err).ToNot(HaveOccurred())
	clusterBootstrapTemplateWebhook := addonwebhooks.ClusterBootstrapTemplate{
		SystemNamespace: SystemNamespace,
	}
	err = clusterBootstrapTemplateWebhook.SetupWebhookWithManager(mgr)
	Expect(err).ToNot(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()

	// wait for the webhook server to get ready
	dialer := &net.Dialer{Timeout: time.Second}
	addrPort := fmt.Sprintf("%s:%d", webhookInstallOptions.LocalServingHost, webhookInstallOptions.LocalServingPort)
	Eventually(func() error {
		// It is safe to have nolint:gosec below is because this is just a test
		conn, err := tls.DialWithDialer(dialer, "tcp", addrPort, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec
		if err != nil {
			return err
		}
		conn.Close()
		return nil
	}).Should(Succeed())

	close(done)
}, 60)

var _ = AfterSuite(func() {
	cancel()
	By("tearing down the test environment")
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

func constructCRDPaths() []string {
	// We do not need all CRDs under config/crd/bases, only load the required ones
	return []string{
		filepath.Join("..", "..", "apis", "addonconfigs", "config", "crd", "bases", "cni.tanzu.vmware.com_antreaconfigs.yaml"),
		filepath.Join("..", "..", "apis", "addonconfigs", "config", "crd", "bases", "cni.tanzu.vmware.com_calicoconfigs.yaml"),
		filepath.Join("..", "..", "apis", "addonconfigs", "config", "crd", "bases", "cpi.tanzu.vmware.com_vspherecpiconfigs.yaml"),
		filepath.Join("..", "..", "apis", "addonconfigs", "config", "crd", "bases", "csi.tanzu.vmware.com_vspherecsiconfigs.yaml"),
		filepath.Join("..", "..", "apis", "addonconfigs", "config", "crd", "bases", "cpi.tanzu.vmware.com_kubevipcpiconfigs.yaml"),
		filepath.Join("..", "..", "apis", "run", "config", "crd", "bases", "run.tanzu.vmware.com_kappcontrollerconfigs.yaml"),
		filepath.Join("..", "..", "apis", "run", "config", "crd", "bases", "run.tanzu.vmware.com_clusterbootstraps.yaml"),
		filepath.Join("..", "..", "apis", "run", "config", "crd", "bases", "run.tanzu.vmware.com_clusterbootstraptemplates.yaml"),
		filepath.Join("testdata", "crds"),
	}
}
