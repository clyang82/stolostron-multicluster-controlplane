// Copyright Contributors to the Open Cluster Management project

package managedclusteraddons

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	hyperv1beta1 "github.com/openshift/hypershift/api/v1beta1"
	"github.com/openshift/library-go/pkg/operator/events"
	hyperagent "github.com/stolostron/hypershift-addon-operator/pkg/agent"
	"github.com/stolostron/hypershift-addon-operator/pkg/metrics"
	"github.com/stolostron/hypershift-addon-operator/pkg/util"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterclientset "open-cluster-management.io/api/client/cluster/clientset/versioned"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	clusterv1alpha1 "open-cluster-management.io/api/cluster/v1alpha1"
	operatorapiv1 "open-cluster-management.io/api/operator/v1"
	"open-cluster-management.io/multicluster-controlplane/pkg/agent"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

var (
	scheme = runtime.NewScheme()
)

// start the hypershift addon agent
// create the managed cluster and policy addon in hosted mode

func InstallHyperShiftAddon(ctx context.Context, o *agent.AgentOptions) error {

	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(hyperv1beta1.AddToScheme(scheme))
	utilruntime.Must(addonv1alpha1.AddToScheme(scheme))
	utilruntime.Must(clusterv1alpha1.AddToScheme(scheme))
	utilruntime.Must(clusterv1.AddToScheme(scheme))
	utilruntime.Must(operatorapiv1.AddToScheme(scheme))

	opts := zap.Options{
		// enable development mode for more human-readable output, extra stack traces and logging information, etc
		// disable this in final release
		Development: true,
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("hypershift-addon-agent")

	spokeConfig, err := clientcmd.BuildConfigFromFlags("", o.RegistrationAgent.SpokeKubeconfig)
	if err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(spokeConfig, ctrl.Options{
		Scheme:             scheme,
		MetricsBindAddress: "0",
		LeaderElection:     false,
	})

	metrics.AddonAgentFailedToStartBool.Set(0)

	if err != nil {
		log.Error(err, "unable to start manager")
		metrics.AddonAgentFailedToStartBool.Set(1)
		return fmt.Errorf("unable to create manager, err: %w", err)
	}

	// build kubeinformerfactory of hub cluster
	hubConfig, err := clientcmd.BuildConfigFromFlags("", o.RegistrationAgent.BootstrapKubeconfig)
	if err != nil {
		metrics.AddonAgentFailedToStartBool.Set(1)
		return fmt.Errorf("failed to create hubConfig from flag, err: %w", err)
	}

	hubClient, err := client.New(hubConfig, client.Options{Scheme: scheme})
	if err != nil {
		metrics.AddonAgentFailedToStartBool.Set(1)
		return fmt.Errorf("failed to create hubClient, err: %w", err)
	}

	spokeKubeClient, err := client.New(spokeConfig, client.Options{Scheme: scheme})
	if err != nil {
		metrics.AddonAgentFailedToStartBool.Set(1)
		return fmt.Errorf("failed to create spoke client, err: %w", err)
	}

	spokeClusterClient, err := clusterclientset.NewForConfig(spokeConfig)
	if err != nil {
		metrics.AddonAgentFailedToStartBool.Set(1)
		return fmt.Errorf("failed to create spoke clusters client, err: %w", err)
	}

	aCtrl := &agentController{
		hubClient:           hubClient,
		spokeUncachedClient: spokeKubeClient,
		spokeClient:         mgr.GetClient(),
		spokeClustersClient: spokeClusterClient,
	}

	// TODO
	//aCtrl.plugInOption(o)

	metrics.InstallationFailningGaugeBool.Set(0)

	// for next-gen controller case, the hypershift operator is pre-installed.
	// Image upgrade controller
	// uCtrl := install.NewUpgradeController(hubClient, spokeKubeClient, o.Log, o.AddonName, o.AddonNamespace, o.SpokeClusterName,
	// 	o.HypershiftOperatorImage, o.PullSecretName, o.WithOverride, ctx)

	// // Perform initial hypershift operator installation on start-up, then start the process to continuously check
	// // if the hypershift operator re-installation is needed
	// uCtrl.Start()

	if err := aCtrl.createManagementClusterClaim(ctx); err != nil {
		metrics.AddonAgentFailedToStartBool.Set(1)
		return fmt.Errorf("unable to create management cluster claim, err: %w", err)
	}

	maxHCNum, thresholdHCNum := aCtrl.getMaxAndThresholdHCCount()
	aCtrl.maxHostedClusterCount = maxHCNum
	aCtrl.thresholdHostedClusterCount = thresholdHCNum
	log.Info("the maximum hosted cluster count set to " + strconv.Itoa(aCtrl.maxHostedClusterCount))
	log.Info("the threshold hosted cluster count set to " + strconv.Itoa(aCtrl.thresholdHostedClusterCount))
	metrics.MaxNumHostedClustersGauge.Set(float64(maxHCNum))
	metrics.ThresholdNumHostedClustersGauge.Set(float64(thresholdHCNum))

	err = aCtrl.SyncAddOnPlacementScore(ctx, true)
	if err != nil {
		// AddOnPlacementScore must be created initially
		metrics.AddonAgentFailedToStartBool.Set(1)
		return fmt.Errorf("failed to create AddOnPlacementScore, err: %w", err)
	}

	log.Info("starting manager")

	//+kubebuilder:scaffold:builder
	if err = aCtrl.SetupWithManager(mgr); err != nil {
		metrics.AddonAgentFailedToStartBool.Set(1)
		return fmt.Errorf("unable to create agent controller: %s, err: %w", util.AddonControllerName, err)
	}

	addonStatusController := &AddonStatusController{
		spokeClient: spokeKubeClient,
		hubClient:   hubClient,
		log:         log.WithName("addon-status-controller"),
		addonNsn:    types.NamespacedName{Namespace: o.RegistrationAgent.ClusterName, Name: util.AddonControllerName},
		clusterName: o.RegistrationAgent.ClusterName,
	}

	if err = addonStatusController.SetupWithManager(mgr); err != nil {
		metrics.AddonAgentFailedToStartBool.Set(1)
		return fmt.Errorf("unable to create agent status controller: %s, err: %w", util.AddonStatusControllerName, err)
	}

	return mgr.Start(ctrl.SetupSignalHandler())
}

type agentController struct {
	hubClient                   client.Client
	spokeUncachedClient         client.Client
	spokeClient                 client.Client              //local for agent
	spokeClustersClient         clusterclientset.Interface // client used to create cluster claim for the hypershift management cluster
	log                         logr.Logger
	recorder                    events.Recorder
	clusterName                 string
	maxHostedClusterCount       int
	thresholdHostedClusterCount int
}

func (c *agentController) plugInOption(o *hyperagent.AgentOptions) {
	c.log = o.Log
	c.clusterName = o.SpokeClusterName
}

func (c *agentController) scaffoldHostedclusterSecrets(hcKey types.NamespacedName) []*corev1.Secret {
	return []*corev1.Secret{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "admin-kubeconfig",
				Namespace: hcKey.Namespace,
				Labels: map[string]string{
					"synced-from-spoke":                  "true",
					util.HypershiftClusterNameLabel:      hcKey.Name,
					util.HypershiftHostingNamespaceLabel: hcKey.Namespace,
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kubeadmin-password",
				Namespace: hcKey.Namespace,
				Labels: map[string]string{
					"synced-from-spoke":                  "true",
					util.HypershiftClusterNameLabel:      hcKey.Name,
					util.HypershiftHostingNamespaceLabel: hcKey.Namespace,
				},
			},
		},
	}
}

func (c *agentController) generateExtManagedKubeconfigSecret(ctx context.Context, secretData map[string][]byte, hc hyperv1beta1.HostedCluster) error {
	// 1. Get hosted cluster's admin kubeconfig secret
	secret := &corev1.Secret{}
	secret.SetName("external-managed-kubeconfig")
	managedClusterAnnoValue, ok := hc.GetAnnotations()[util.ManagedClusterAnnoKey]
	if !ok || len(managedClusterAnnoValue) == 0 {
		managedClusterAnnoValue = hc.Name
	}
	secret.SetNamespace("klusterlet-" + managedClusterAnnoValue)
	kubeconfigData := secretData["kubeconfig"]

	klusterletNamespace := &corev1.Namespace{}
	klusterletNamespaceNsn := types.NamespacedName{Name: "klusterlet-" + managedClusterAnnoValue}

	err := c.spokeClient.Get(ctx, klusterletNamespaceNsn, klusterletNamespace)
	if err != nil {
		c.log.Error(err, fmt.Sprintf("failed to find the klusterlet namespace: %s ", klusterletNamespaceNsn.Name))
		return fmt.Errorf("failed to find the klusterlet namespace: %s", klusterletNamespaceNsn.Name)
	}

	if kubeconfigData == nil {
		return fmt.Errorf("failed to get kubeconfig from secret: %s", secret.GetName())
	}

	kubeconfig, err := clientcmd.Load(kubeconfigData)

	if err != nil {
		c.log.Error(err, fmt.Sprintf("failed to load kubeconfig from secret: %s", secret.GetName()))
		return fmt.Errorf("failed to load kubeconfig from secret: %s", secret.GetName())
	}

	if len(kubeconfig.Clusters) == 0 {
		c.log.Error(err, fmt.Sprintf("there is no cluster in kubeconfig from secret: %s", secret.GetName()))
		return fmt.Errorf("there is no cluster in kubeconfig from secret: %s", secret.GetName())
	}

	if kubeconfig.Clusters["cluster"] == nil {
		c.log.Error(err, fmt.Sprintf("failed to get a cluster from kubeconfig in secret: %s", secret.GetName()))
		return fmt.Errorf("failed to get a cluster from kubeconfig in secret: %s", secret.GetName())
	}

	// 2. Get the kube-apiserver service port
	apiServicePort, err := c.getAPIServicePort(hc)
	if err != nil {
		c.log.Error(err, "failed to get the kube api service port")
		return err
	}

	// 3. Replace the config.Clusters["cluster"].Server URL with internal kubeadpi service URL kube-apiserver.<Namespace>.svc.cluster.local
	apiServerURL := "https://kube-apiserver." + hc.Namespace + "-" + hc.Name + ".svc.cluster.local:" + apiServicePort
	kubeconfig.Clusters["cluster"].Server = apiServerURL

	newKubeconfig, err := clientcmd.Write(*kubeconfig)

	if err != nil {
		c.log.Error(err, fmt.Sprintf("failed to write new kubeconfig to secret: %s", secret.GetName()))
		return fmt.Errorf("failed to write new kubeconfig to secret: %s", secret.GetName())
	}

	secretData["kubeconfig"] = newKubeconfig

	secret.Data = secretData

	c.log.Info("Set the cluster server URL in external-managed-kubeconfig secret", "apiServerURL", apiServerURL)

	nilFunc := func() error { return nil }

	// 3. Create the admin kubeconfig secret as external-managed-kubeconfig in klusterlet-<infraID> namespace
	_, err = controllerutil.CreateOrUpdate(ctx, c.spokeClient, secret, nilFunc)
	if err != nil {
		c.log.Error(err, "failed to createOrUpdate external-managed-kubeconfig secret", "secret", client.ObjectKeyFromObject(secret))
		return err
	}

	c.log.Info("createOrUpdate external-managed-kubeconfig secret", "secret", client.ObjectKeyFromObject(secret))

	return nil
}

func (c *agentController) getAPIServicePort(hc hyperv1beta1.HostedCluster) (string, error) {
	apiService := &corev1.Service{}
	apiServiceNsn := types.NamespacedName{Namespace: hc.Namespace + "-" + hc.Name, Name: "kube-apiserver"}
	err := c.spokeClient.Get(context.TODO(), apiServiceNsn, apiService)
	if err != nil {
		c.log.Error(err, "failed to find kube-apiserver service for the hosted cluster")
		return "", err
	}

	apiServicePort := apiService.Spec.Ports[0].Port

	return strconv.FormatInt(int64(apiServicePort), 10), nil
}

func (c *agentController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	c.log.Info(fmt.Sprintf("Reconciling hostedcluster secrect %s", req))
	defer c.log.Info(fmt.Sprintf("Done reconcile hostedcluster secrect %s", req))

	// Update the AddOnPlacementScore resource, requeue reconcile if error occurred
	metrics.TotalReconcileCount.Inc() // increase reconcile action count
	if err := c.SyncAddOnPlacementScore(ctx, false); err != nil {
		c.log.Info(fmt.Sprintf("failed to create or update ethe AddOnPlacementScore %s, error: %s. Will try again in 30 seconds", util.HostedClusterScoresResourceName, err.Error()))
		metrics.ReconcileRequeueCount.Inc()
		metrics.FailedReconcileCount.Inc()
		return ctrl.Result{Requeue: true, RequeueAfter: time.Duration(1) * time.Minute}, nil
	}

	// Delete HC secrets on the hub using labels for HC and the hosting NS
	deleteMirrorSecrets := func() error {
		secretSelector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
			MatchLabels: map[string]string{
				util.HypershiftClusterNameLabel:      req.Name,
				util.HypershiftHostingNamespaceLabel: req.Namespace,
			},
		})
		if err != nil {
			c.log.Error(err, fmt.Sprintf("failed to convert label to get secrets on hub for hostedCluster: %s", req))
			return err
		}

		listopts := &client.ListOptions{}
		listopts.LabelSelector = secretSelector
		listopts.Namespace = c.clusterName
		hcHubSecretList := &corev1.SecretList{}
		err = c.hubClient.List(ctx, hcHubSecretList, listopts)
		if err != nil {
			c.log.Error(err, fmt.Sprintf("failed to get secrets on hub for hostedCluster: %s", req))
			return err
		}

		var lastErr error
		for i := range hcHubSecretList.Items {
			se := hcHubSecretList.Items[i]
			c.log.V(4).Info(fmt.Sprintf("deleting secret(%s) on hub", client.ObjectKeyFromObject(&se)))
			if err := c.hubClient.Delete(ctx, &se); err != nil && !apierrors.IsNotFound(err) {
				lastErr = err
				c.log.Error(err, fmt.Sprintf("failed to delete secret(%s) on hub", client.ObjectKeyFromObject(&se)))
			}
		}

		return lastErr
	}

	hc := &hyperv1beta1.HostedCluster{}
	if err := c.spokeClient.Get(ctx, req.NamespacedName, hc); err != nil {
		if apierrors.IsNotFound(err) {
			c.log.Info(fmt.Sprintf("remove hostedcluster(%s) secrets on hub, since hostedcluster is gone", req.NamespacedName))

			return ctrl.Result{}, deleteMirrorSecrets()
		}

		c.log.Error(err, "failed to get the hostedcluster")
		return ctrl.Result{}, nil
	}

	if !hc.GetDeletionTimestamp().IsZero() {
		c.log.Info(fmt.Sprintf("hostedcluster %s has deletionTimestamp %s. Skip reconciling klusterlet secrets", hc.Name, hc.GetDeletionTimestamp().String()))

		if err := c.deleteManagedCluster(ctx, hc); err != nil {
			c.log.Error(err, "failed to delete the managed cluster")
		}

		return ctrl.Result{}, nil
	}

	if hc.Status.Conditions == nil || len(hc.Status.Conditions) == 0 ||
		!c.isHostedControlPlaneAvailable(hc.Status) {
		// Wait for secrets to exist
		c.log.Info(fmt.Sprintf("hostedcluster %s's control plane is not ready yet.", hc.Name))
		return ctrl.Result{}, nil
	}

	adminKubeConfigSecretWithCert := &corev1.Secret{}

	createOrUpdateMirrorSecrets := func() error {
		var lastErr error
		managedClusterAnnoValue, ok := hc.GetAnnotations()[util.ManagedClusterAnnoKey]
		if !ok || len(managedClusterAnnoValue) == 0 {
			c.log.Info("did not find managed cluster's name annotation from hosted cluster, using infra-id")
			managedClusterAnnoValue = hc.Name
			ok = true
		}

		hcSecrets := c.scaffoldHostedclusterSecrets(req.NamespacedName)
		for _, se := range hcSecrets {
			secretName := hc.Spec.InfraID
			if ok && len(managedClusterAnnoValue) > 0 {
				secretName = managedClusterAnnoValue
			}
			hubMirrorSecret := se.DeepCopy()
			hubMirrorSecret.SetNamespace(c.clusterName)
			hubMirrorSecret.SetName(fmt.Sprintf("%s-%s", secretName, se.Name))

			if strings.HasSuffix(hubMirrorSecret.Name, "kubeadmin-password") {
				if hc.Status.KubeadminPassword == nil {
					// the kubeadmin password secret is not ready yet
					// this secret will not be created if a custom identity provider
					// is configured in configuration.oauth.identityProviders
					c.log.Info("cannot find the kubeadmin password secret yet.")
					continue
				}
			}

			se.SetName(fmt.Sprintf("%s-%s", hc.Name, se.Name))
			if err := c.spokeClient.Get(ctx, client.ObjectKeyFromObject(se), se); err != nil {
				lastErr = err
				c.log.Error(err, fmt.Sprintf("failed to get hosted cluster secret %s on local cluster, skip this one", client.ObjectKeyFromObject(se)))
				continue
			}

			hubMirrorSecret.SetAnnotations(map[string]string{util.ManagedClusterAnnoKey: managedClusterAnnoValue})
			hubMirrorSecret.Data = se.Data

			if strings.HasSuffix(hubMirrorSecret.Name, "admin-kubeconfig") {
				// Create or update external-managed-kubeconfig secret for managed cluster registration agent
				c.log.Info("Generating external-managed-kubeconfig secret")

				extSecret := se.DeepCopy()

				errExt := c.generateExtManagedKubeconfigSecret(ctx, extSecret.Data, *hc)

				if errExt != nil {
					// This is where we avoid counting metrics for certain error conditions
					// Klusterlet namespace will not exist until import is done
					if !strings.Contains(errExt.Error(), "failed to find the klusterlet namespace") {
						metrics.KubeconfigSecretCopyFailureCount.Inc()
						lastErr = errExt // it is an error condition only if the klueterlet namespace exists
					}

				} else {
					c.log.Info("Successfully generated external-managed-kubeconfig secret")
				}

				// Replace certificate-authority-data from admin-kubeconfig
				servingCert := getServingCert(hc)
				if servingCert != "" {
					kubeconfig := hubMirrorSecret.Data["kubeconfig"]

					updatedKubeconfig, err := c.replaceCertAuthDataInKubeConfig(ctx, kubeconfig, hc.Namespace, servingCert)
					if err != nil {
						lastErr = err
						c.log.Info("failed to replace certificate-authority-data from kubeconfig")
						continue
					}

					c.log.Info(fmt.Sprintf("Replaced certificate-authority-data from secret: %v", hubMirrorSecret.Name))

					hubMirrorSecret.Data["kubeconfig"] = updatedKubeconfig
				}

				// Save this admin kubeconfig secret to use later to create the cluster claim
				// which requires connection to the hosted cluster's API server
				adminKubeConfigSecretWithCert = hubMirrorSecret
			}

			mutateFunc := func(secret *corev1.Secret, data map[string][]byte) controllerutil.MutateFn {
				return func() error {
					secret.Data = data
					return nil
				}
			}

			_, err := controllerutil.CreateOrUpdate(ctx, c.hubClient, hubMirrorSecret, mutateFunc(hubMirrorSecret, hubMirrorSecret.Data))
			if err != nil {
				lastErr = err
				c.log.Error(err, fmt.Sprintf("failed to createOrUpdate hostedcluster secret %s to hub", client.ObjectKeyFromObject(hubMirrorSecret)))
			} else {
				c.log.Info(fmt.Sprintf("createOrUpdate hostedcluster secret %s to hub", client.ObjectKeyFromObject(hubMirrorSecret)))
			}

		}

		return lastErr
	}

	metrics.TotalReconcileCount.Inc() // increase reconcile action count
	if err := createOrUpdateMirrorSecrets(); err != nil {
		c.log.Info(fmt.Sprintf("failed to create external-managed-kubeconfig and mirror secrets for hostedcluster %s, error: %s. Will try again in 30 seconds", hc.Name, err.Error()))
		metrics.FailedReconcileCount.Inc()
		return ctrl.Result{Requeue: true, RequeueAfter: time.Duration(1) * time.Minute}, nil
	}

	if isVersionHistoryStateFound(hc.Status.Version.History, configv1.CompletedUpdate) {
		if err := c.createHostedClusterClaim(ctx, adminKubeConfigSecretWithCert,
			generateClusterClientFromSecret); err != nil {
			// just log the infomation and wait for the next reconcile to retry.
			// since the hosted cluster may:
			//   - not available now
			//   - have not been imported to the hub, and there is no clusterclaim CRD.
			c.log.Info("unable to create hosted cluster claim, wait for the next retry", "error", err.Error())
			// this is not critical for managing hosted clusters. don't count as a failed reconcile
			metrics.ReconcileRequeueCount.Inc()
			return ctrl.Result{Requeue: true, RequeueAfter: time.Duration(1) * time.Minute}, nil
		}
	}

	return ctrl.Result{}, nil
}

func (c *agentController) isHostedControlPlaneAvailable(status hyperv1beta1.HostedClusterStatus) bool {
	for _, condition := range status.Conditions {
		if condition.Reason == hyperv1beta1.AsExpectedReason && condition.Status == metav1.ConditionTrue && condition.Type == string(hyperv1beta1.HostedClusterAvailable) {
			return true
		}
	}
	return false
}

func isVersionHistoryStateFound(history []configv1.UpdateHistory, state configv1.UpdateState) bool {
	for _, h := range history {
		if h.State == state {
			return true
		}
	}
	return false
}

func (c *agentController) SyncAddOnPlacementScore(ctx context.Context, startup bool) error {
	addOnPlacementScore := &clusterv1alpha1.AddOnPlacementScore{
		TypeMeta: metav1.TypeMeta{
			Kind:       "AddOnPlacementScore",
			APIVersion: "cluster.open-cluster-management.io/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      util.HostedClusterScoresResourceName,
			Namespace: c.clusterName,
		},
	}

	_, err := controllerutil.CreateOrUpdate(context.TODO(), c.hubClient, addOnPlacementScore, func() error { return nil })
	if err != nil {
		// just log the error. it should not stop the rest of reconcile
		c.log.Error(err, fmt.Sprintf("failed to create or update the addOnPlacementScore resource in %s", c.clusterName))
		// Emit metrics to return the number of placement score update failures
		metrics.PlacementScoreFailureCount.Inc()
		return err
	}

	listopts := &client.ListOptions{}
	hcList := &hyperv1beta1.HostedClusterList{}
	err = c.spokeUncachedClient.List(context.TODO(), hcList, listopts)
	hcCRDNotInstalledYet := err != nil &&
		(strings.HasPrefix(err.Error(), "no matches for kind ") || strings.HasPrefix(err.Error(), "no kind is registered ")) &&
		startup
	if hcCRDNotInstalledYet {
		c.log.Info("this is the initial agent startup and the hypershift CRDs are not installed yet, " + err.Error())
		c.log.Info("going to continue updating AddOnPlacementScore and cluster claims with zero HC count")
	}
	// During the first agent startup on a brand new cluster, the hypershift operator and its CRDs will not be installed yet.
	// So listing the HCs will fail. In this case, just set the count to len(hcList.Items) which is zero.
	if err != nil && !hcCRDNotInstalledYet {
		// just log the error. it should not stop the rest of reconcile
		c.log.Error(err, "failed to get HostedCluster list")

		meta.SetStatusCondition(&addOnPlacementScore.Status.Conditions, metav1.Condition{
			Type:    "HostedClusterCountUpdated",
			Status:  metav1.ConditionFalse,
			Reason:  "HostedClusterCountFailed",
			Message: err.Error(),
		})

		err = c.hubClient.Status().Update(context.TODO(), addOnPlacementScore, &client.UpdateOptions{})
		if err != nil {
			// just log the error. it should not stop the rest of reconcile
			c.log.Error(err, fmt.Sprintf("failed to update the addOnPlacementScore status in %s", c.clusterName))
			// Emit metrics to return the number of placement score update failures
			metrics.PlacementScoreFailureCount.Inc()
			return err
		}
	} else {
		hcCount := len(hcList.Items)
		scores := []clusterv1alpha1.AddOnPlacementScoreItem{
			{
				Name:  util.HostedClusterScoresScoreName,
				Value: int32(hcCount),
			},
		}

		// Total number of hosted clusters metric
		metrics.TotalHostedClusterGauge.Set(float64(hcCount))

		availableHcpNum := 0
		completedHcNum := 0
		deletingHcNum := 0

		for _, hc := range hcList.Items {
			if hc.Status.Conditions == nil || len(hc.Status.Conditions) == 0 || c.isHostedControlPlaneAvailable(hc.Status) {
				availableHcpNum++
			}

			if hc.Status.Version == nil || len(hc.Status.Version.History) == 0 ||
				isVersionHistoryStateFound(hc.Status.Version.History, configv1.CompletedUpdate) {
				completedHcNum++
			}

			if !hc.GetDeletionTimestamp().IsZero() {
				deletingHcNum++
			}
		}

		// Total number of available hosted control plains metric
		metrics.HostedControlPlaneAvailableGauge.Set(float64(availableHcpNum))
		// Total number of completed hosted clusters metric
		metrics.HostedClusterAvailableGauge.Set(float64(completedHcNum))
		// Total number of hosted clusters being deleted
		metrics.HostedClusterBeingDeletedGauge.Set(float64(deletingHcNum))

		meta.SetStatusCondition(&addOnPlacementScore.Status.Conditions, metav1.Condition{
			Type:    "HostedClusterCountUpdated",
			Status:  metav1.ConditionTrue,
			Reason:  "HostedClusterCountUpdated",
			Message: "Hosted cluster count was updated successfully",
		})
		addOnPlacementScore.Status.Scores = scores

		err = c.hubClient.Status().Update(context.TODO(), addOnPlacementScore, &client.UpdateOptions{})
		if err != nil {
			// just log the error. it should not stop the rest of reconcile
			c.log.Error(err, fmt.Sprintf("failed to update the addOnPlacementScore status in %s", c.clusterName))
			// Emit metrics to return the number of placement score update failures
			metrics.PlacementScoreFailureCount.Inc()
			return err
		}

		c.log.Info(fmt.Sprintf("updated the addOnPlacementScore for %s: %v", c.clusterName, hcCount))

		// Based on the new HC count, update the zero, threshold, full cluster claim values.
		if err := c.createHostedClusterFullClusterClaim(ctx, hcCount); err != nil {
			c.log.Error(err, "failed to create or update hosted cluster full cluster claim")
			metrics.PlacementClusterClaimsFailureCount.WithLabelValues(util.MetricsLabelFullClusterClaim).Inc()
			return err
		}

		if err = c.createHostedClusterThresholdClusterClaim(ctx, hcCount); err != nil {
			c.log.Error(err, "failed to create or update hosted cluster threshold cluster claim")
			metrics.PlacementClusterClaimsFailureCount.WithLabelValues(util.MetricsLabelThresholdClusterClaim).Inc()
			return err
		}

		if err = c.createHostedClusterZeroClusterClaim(ctx, hcCount); err != nil {
			c.log.Error(err, "failed to create hosted cluster zero cluster claim")
			metrics.PlacementClusterClaimsFailureCount.WithLabelValues(util.MetricsLabelZeroClusterClaim).Inc()
			return err
		}

		c.log.Info("updated the hosted cluster cound cluster claims successfully")
	}

	return nil
}

func (c *agentController) deleteManagedCluster(ctx context.Context, hc *hyperv1beta1.HostedCluster) error {
	if hc == nil {
		return fmt.Errorf("failed to delete nil hostedCluster")
	}

	managedClusterName, ok := hc.GetAnnotations()[util.ManagedClusterAnnoKey]
	if !ok || len(managedClusterName) == 0 {
		managedClusterName = hc.Name
	}

	// Delete the managed cluster
	mc := &clusterv1.ManagedCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: managedClusterName,
		},
	}

	if err := c.hubClient.Get(ctx, client.ObjectKeyFromObject(mc), mc); err != nil {
		if apierrors.IsNotFound(err) {
			c.log.Info(fmt.Sprintf("managedCluster %v is already deleted", managedClusterName))
			mc = nil
		} else {
			c.log.Info(fmt.Sprintf("failed to get the managedCluster %v", managedClusterName))
			return err
		}
	}

	if mc != nil {
		if err := c.hubClient.Delete(ctx, mc); err != nil {
			c.log.Info(fmt.Sprintf("failed to delete the managedCluster %v", managedClusterName))
			return err
		}

		c.log.Info(fmt.Sprintf("deleted managedCluster %v", managedClusterName))
	}

	klusterletName := "klusterlet-" + managedClusterName

	// Remove the operator.open-cluster-management.io/klusterlet-hosted-cleanup finalizer in klusterlet
	klusterlet := &operatorapiv1.Klusterlet{
		ObjectMeta: metav1.ObjectMeta{
			Name: klusterletName,
		},
	}

	if err := c.spokeUncachedClient.Get(ctx, client.ObjectKeyFromObject(klusterlet), klusterlet); err != nil {
		if apierrors.IsNotFound(err) {
			c.log.Info(fmt.Sprintf("klusterlet %v is already deleted", klusterletName))
			return nil
		} else {
			c.log.Info(fmt.Sprintf("failed to get the klusterlet %v", klusterletName))
			return err
		}
	}

	updated := controllerutil.RemoveFinalizer(klusterlet, "operator.open-cluster-management.io/klusterlet-hosted-cleanup")
	c.log.Info(fmt.Sprintf("klusterlet %v finalizer removed:%v", klusterletName, updated))

	if updated {
		if err := c.spokeUncachedClient.Update(ctx, klusterlet); err != nil {
			c.log.Info("failed to update klusterlet to remove the finalizer")
			return err
		}
	}

	return nil
}

func (c *agentController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&hyperv1beta1.HostedCluster{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(c)
}

func (c *agentController) replaceCertAuthDataInKubeConfig(ctx context.Context, kubeconfig []byte, certNs, certName string) ([]byte, error) {
	secret := &corev1.Secret{}
	if err := c.spokeClient.Get(ctx, types.NamespacedName{Namespace: certNs, Name: certName}, secret); err != nil {
		c.log.Info(fmt.Sprintf("failed to get secret for serving certificate %v/%v", certNs, certName))

		return nil, err
	}

	tlsCrt := secret.Data["tls.crt"]
	if tlsCrt == nil {
		err := fmt.Errorf("invalid serving certificate secret")
		c.log.Info(err.Error())

		return nil, err
	}

	config, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return nil, err
	}

	for _, v := range config.Clusters {
		v.CertificateAuthorityData = tlsCrt
	}

	updatedConfig, err := clientcmd.Write(*config)
	if err != nil {
		return nil, err
	}

	return updatedConfig, nil
}

// Retrieves the first serving certificate
func getServingCert(hc *hyperv1beta1.HostedCluster) string {
	if hc.Spec.Configuration != nil &&
		hc.Spec.Configuration.APIServer != nil &&
		&hc.Spec.Configuration.APIServer.ServingCerts != nil &&
		len(hc.Spec.Configuration.APIServer.ServingCerts.NamedCertificates) > 0 {
		return hc.Spec.Configuration.APIServer.ServingCerts.NamedCertificates[0].ServingCertificate.Name
	}

	return ""
}

const (
	// labelExcludeBackup is true for the local-cluster will not be backed up into velero
	labelExcludeBackup = "velero.io/exclude-from-backup"

	hypershiftManagementClusterClaimKey             = "hostingcluster.hypershift.openshift.io"
	hypershiftHostedClusterClaimKey                 = "hostedcluster.hypershift.openshift.io"
	hostedClusterCountFullClusterClaimKey           = "full.hostedclustercount.hypershift.openshift.io"
	hostedClusterCountAboveThresholdClusterClaimKey = "above.threshold.hostedclustercount.hypershift.openshift.io"
	hostedClusterCountZeroClusterClaimKey           = "zero.hostedclustercount.hypershift.openshift.io"
)

func newClusterClaim(name, value string) *clusterv1alpha1.ClusterClaim {
	return &clusterv1alpha1.ClusterClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{labelExcludeBackup: "true"},
		},
		Spec: clusterv1alpha1.ClusterClaimSpec{
			Value: value,
		},
	}
}

func createOrUpdate(ctx context.Context, client clusterclientset.Interface, newClaim *clusterv1alpha1.ClusterClaim) error {
	oldClaim, err := client.ClusterV1alpha1().ClusterClaims().Get(ctx, newClaim.Name, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		_, err := client.ClusterV1alpha1().ClusterClaims().Create(ctx, newClaim, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("unable to create ClusterClaim: %v, %w", newClaim, err)
		}
	case err != nil:
		return fmt.Errorf("unable to get ClusterClaim %q: %w", newClaim.Name, err)
	case !reflect.DeepEqual(oldClaim.Spec, newClaim.Spec):
		oldClaim.Spec = newClaim.Spec
		_, err := client.ClusterV1alpha1().ClusterClaims().Update(ctx, oldClaim, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("unable to update ClusterClaim %q: %w", oldClaim.Name, err)
		}
	}

	return nil
}

func (c *agentController) createManagementClusterClaim(ctx context.Context) error {
	managementClaim := newClusterClaim(hypershiftManagementClusterClaimKey, "true")
	return createOrUpdate(ctx, c.spokeClustersClient, managementClaim)
}

func (c *agentController) createHostedClusterFullClusterClaim(ctx context.Context, count int) error {
	if count >= c.maxHostedClusterCount {
		c.log.Info(fmt.Sprintf("ATTENTION: the hosted cluster count has reached the maximum %s.", strconv.Itoa(c.maxHostedClusterCount)))
	} else {
		c.log.Info(fmt.Sprintf("the hosted cluster count has not reached the maximum %s yet. current count is %s", strconv.Itoa(c.maxHostedClusterCount), strconv.Itoa(count)))
	}
	hcFullClaim := newClusterClaim(hostedClusterCountFullClusterClaimKey, strconv.FormatBool(count >= c.maxHostedClusterCount))
	return createOrUpdate(ctx, c.spokeClustersClient, hcFullClaim)
}

func (c *agentController) createHostedClusterThresholdClusterClaim(ctx context.Context, count int) error {
	hcThresholdClaim := newClusterClaim(hostedClusterCountAboveThresholdClusterClaimKey, strconv.FormatBool(count >= c.thresholdHostedClusterCount))
	return createOrUpdate(ctx, c.spokeClustersClient, hcThresholdClaim)
}

func (c *agentController) createHostedClusterZeroClusterClaim(ctx context.Context, count int) error {
	hcZeroClaim := newClusterClaim(hostedClusterCountZeroClusterClaimKey, strconv.FormatBool(count == 0))
	return createOrUpdate(ctx, c.spokeClustersClient, hcZeroClaim)
}

func (c *agentController) createHostedClusterClaim(ctx context.Context, secret *corev1.Secret,
	generateClusterClientFromSecret func(secret *corev1.Secret) (clusterclientset.Interface, error)) error {
	if secret.Data == nil {
		return fmt.Errorf("the secret does not have any data")
	}
	clusterClient, err := generateClusterClientFromSecret(secret)
	if err != nil {
		return fmt.Errorf("failed to create spoke clusters client, err: %w", err)
	}

	hostedClaim := newClusterClaim(hypershiftHostedClusterClaimKey, "true")
	err = createOrUpdate(ctx, clusterClient, hostedClaim)
	if err != nil {
		return err
	}
	return nil
}

func generateClusterClientFromSecret(secret *corev1.Secret) (clusterclientset.Interface, error) {
	config, err := util.GenerateClientConfigFromSecret(secret)
	if err != nil {
		return nil, fmt.Errorf("unable to generate client config from secret: %w", err)
	}

	clusterClient, err := clusterclientset.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create spoke clusters client, err: %w", err)
	}

	return clusterClient, nil
}

// As the number of hosted cluster count reaches the max and threshold hosted cluster counts
// are used to generate two cluster claims:
// "above.threshold.hostedclustercount.hypershift.openshift.io" = true when the count > threshold
// "full.hostedclustercount.hypershift.openshift.io" = true when the count >= max
// Both max and threshold numbers should be valid positive integer numbers and max >= threshold.
// If not, they default to 80 max and 60 threshold.
func (c *agentController) getMaxAndThresholdHCCount() (int, int) {
	maxNum := util.DefaultMaxHostedClusterCount
	envMax := os.Getenv("HC_MAX_NUMBER")
	if envMax == "" {
		c.log.Info("env variable HC_MAX_NUMBER not found, defaulting to 80")
	}

	maxNum, err := strconv.Atoi(envMax)
	if err != nil {
		c.log.Error(nil, fmt.Sprintf("failed to convert env variable HC_MAX_NUMBER %s to integer, defaulting to 80", envMax))
		maxNum = util.DefaultMaxHostedClusterCount
	}

	if maxNum < 1 {
		c.log.Error(nil, fmt.Sprintf("invalid HC_MAX_NUMBER %s, defaulting to 80", envMax))
		maxNum = util.DefaultMaxHostedClusterCount
	}

	thresholdNum := util.DefaultThresholdHostedClusterCount
	envThreshold := os.Getenv("HC_THRESHOLD_NUMBER")
	if envThreshold == "" {
		c.log.Info("env variable HC_THRESHOLD_NUMBER not found, defaulting to 60")
	}

	thresholdNum, err = strconv.Atoi(envThreshold)
	if err != nil {
		c.log.Error(nil, fmt.Sprintf("failed to convert env variable HC_THRESHOLD_NUMBER %s to integer, defaulting to 60", envThreshold))
		thresholdNum = util.DefaultThresholdHostedClusterCount
	}

	if thresholdNum < 1 {
		c.log.Error(nil, fmt.Sprintf("invalid HC_MAX_NUMBER %s, defaulting to 60", envThreshold))
		thresholdNum = util.DefaultThresholdHostedClusterCount
	}

	if maxNum < thresholdNum {
		c.log.Error(nil, fmt.Sprintf(
			"invalid HC_MAX_NUMBER %s HC_THRESHOLD_NUMBER %s: HC_MAX_NUMBER must be equal or bigger than HC_THRESHOLD_NUMBER, defaulting to 80 and 60 for max and threshold counts",
			envMax, envThreshold))
		maxNum = util.DefaultMaxHostedClusterCount
		thresholdNum = util.DefaultThresholdHostedClusterCount
	}

	return maxNum, thresholdNum
}

var (
	operatorDeploymentNsn    = types.NamespacedName{Namespace: util.HypershiftOperatorNamespace, Name: util.HypershiftOperatorName}
	externalDNSDeploymentNsn = types.NamespacedName{Namespace: util.HypershiftOperatorNamespace, Name: util.HypershiftOperatorExternalDNSName}
)

// AddonStatusController reconciles Hypershift addon status
type AddonStatusController struct {
	spokeClient client.Client
	hubClient   client.Client
	log         logr.Logger
	addonNsn    types.NamespacedName
	clusterName string
}

// AddonStatusPredicateFunctions defines which Deployment this controller should watch
var AddonStatusPredicateFunctions = predicate.Funcs{
	CreateFunc: func(e event.CreateEvent) bool {
		deployment := e.Object.(*appsv1.Deployment)
		return containsHypershiftAddonDeployment(*deployment)
	},
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldDeployment := e.ObjectOld.(*appsv1.Deployment)
		newDeployment := e.ObjectNew.(*appsv1.Deployment)
		return containsHypershiftAddonDeployment(*oldDeployment) && containsHypershiftAddonDeployment(*newDeployment)
	},
	DeleteFunc: func(e event.DeleteEvent) bool {
		deployment := e.Object.(*appsv1.Deployment)
		return containsHypershiftAddonDeployment(*deployment)
	},
}

// SetupWithManager sets up the controller with the Manager.
func (c *AddonStatusController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}).
		WithEventFilter(AddonStatusPredicateFunctions).
		Complete(c)
}

// Reconcile updates the Hypershift addon status based on the Deployment status.
func (c *AddonStatusController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	c.log.Info(fmt.Sprintf("reconciling Deployment %s", req))
	defer c.log.Info(fmt.Sprintf("done reconcile Deployment %s", req))

	checkExtDNS, err := c.shouldCheckExternalDNSDeployment(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	operatorDeployment, err := c.getDeployment(ctx, operatorDeploymentNsn)
	if err != nil {
		return ctrl.Result{}, err
	}

	var externalDNSDeployment *appsv1.Deployment
	if checkExtDNS {
		externalDNSDeployment, err = c.getDeployment(ctx, externalDNSDeploymentNsn)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// generate addon status condition based on the deployments
	addonCondition := checkDeployments(checkExtDNS, operatorDeployment, externalDNSDeployment)

	// update the addon status
	updated, err := c.updateStatus(
		ctx, updateConditionFn(&addonCondition))
	if err != nil {
		c.log.Error(err, "failed to update the addon status")
		return ctrl.Result{}, err
	}

	if updated {
		c.log.Info("updated ManagedClusterAddOnStatus")
	} else {
		c.log.V(4).Info("skip updating updated ManagedClusterAddOnStatus")
	}

	return ctrl.Result{}, nil
}

func (c *AddonStatusController) shouldCheckExternalDNSDeployment(ctx context.Context) (bool, error) {
	extDNSSecretKey := types.NamespacedName{Name: util.HypershiftExternalDNSSecretName, Namespace: c.clusterName}
	sExtDNS := &corev1.Secret{}
	err := c.hubClient.Get(ctx, extDNSSecretKey, sExtDNS)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.log.V(4).Info(fmt.Sprintf("external dns secret(%s) was not found", extDNSSecretKey))
			return false, nil
		}

		c.log.Error(err, fmt.Sprintf("failed to get the external dns secret(%s)", extDNSSecretKey))
		return false, err
	}

	c.log.Info(fmt.Sprintf("found external dns secret(%s)", extDNSSecretKey))
	return true, nil
}

func (c *AddonStatusController) getDeployment(ctx context.Context, nsn types.NamespacedName) (
	*appsv1.Deployment, error) {
	deployment := &appsv1.Deployment{}
	err := c.spokeClient.Get(ctx, nsn, deployment)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
		return nil, nil
	}

	return deployment, nil
}

type UpdateStatusFunc func(status *addonv1alpha1.ManagedClusterAddOnStatus)

func (c *AddonStatusController) updateStatus(ctx context.Context, updateFuncs ...UpdateStatusFunc) (
	bool, error) {
	updated := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		hypershiftAddon := &addonv1alpha1.ManagedClusterAddOn{}
		err := c.hubClient.Get(ctx, c.addonNsn, hypershiftAddon)
		if apierrors.IsNotFound(err) {
			return nil
		}

		if err != nil {
			return err
		}

		oldStatus := &hypershiftAddon.Status

		newStatus := oldStatus.DeepCopy()
		for _, update := range updateFuncs {
			update(newStatus)
		}

		if equality.Semantic.DeepEqual(oldStatus, newStatus) {
			return nil
		}

		hypershiftAddon.Status = *newStatus
		err = c.hubClient.Status().Update(ctx, hypershiftAddon, &client.UpdateOptions{})
		if err != nil {
			return err
		}

		updated = err == nil

		return err
	})

	return updated, err
}

func updateConditionFn(cond *metav1.Condition) UpdateStatusFunc {
	return func(oldStatus *addonv1alpha1.ManagedClusterAddOnStatus) {
		meta.SetStatusCondition(&oldStatus.Conditions, *cond)
	}
}

func containsHypershiftAddonDeployment(deployment appsv1.Deployment) bool {
	if len(deployment.Name) == 0 || len(deployment.Namespace) == 0 {
		return false
	}

	if deployment.Namespace != util.HypershiftOperatorNamespace {
		return false
	}

	return deployment.Name == util.HypershiftOperatorName ||
		deployment.Name == util.HypershiftOperatorExternalDNSName
}

func checkDeployments(checkExtDNSDeploy bool,
	operatorDeployment, externalDNSDeployment *appsv1.Deployment) metav1.Condition {
	reason := ""
	message := ""

	// Emit metrics to indicate that hypershift operator is NOT degraded
	metrics.IsHypershiftOperatorDegraded.Set(0)
	if operatorDeployment == nil {
		reason = degradedReasonOperatorNotFound
		message = degradedReasonOperatorNotFoundMessage
		// Emit metrics to indicate that hypershift operator is degraded
		metrics.IsHypershiftOperatorDegraded.Set(1)
	} else if !operatorDeployment.GetDeletionTimestamp().IsZero() {
		reason = degradedReasonOperatorDeleted
		message = degradedReasonOperatorDeletedMessage
		// Emit metrics to indicate that hypershift operator is degraded
		metrics.IsHypershiftOperatorDegraded.Set(1)
	} else if operatorDeployment.Status.AvailableReplicas == 0 ||
		(operatorDeployment.Spec.Replicas != nil && *operatorDeployment.Spec.Replicas != operatorDeployment.Status.AvailableReplicas) {
		reason = degradedReasonOperatorNotAllAvailableReplicas
		message = degradedReasonOperatorNotAllAvailableReplicasMessage
		// Emit metrics to indicate that hypershift operator is degraded
		metrics.IsHypershiftOperatorDegraded.Set(1)
	}

	// Emit metrics to indicate that external DNS operator is NOT degraded
	metrics.IsExtDNSOperatorDegraded.Set(0)
	if checkExtDNSDeploy {
		isReasonPopulated := len(reason) > 0
		if externalDNSDeployment == nil {
			if isReasonPopulated {
				reason += ","
				message += "\n"
			}
			reason += degradedReasonExternalDNSNotFound
			message += degradedReasonExternalDNSNotFoundMessage
			// Emit metrics to indicate that external DNS operator is degraded
			metrics.IsExtDNSOperatorDegraded.Set(1)
		} else if !externalDNSDeployment.GetDeletionTimestamp().IsZero() {
			if isReasonPopulated {
				reason += ","
				message += "\n"
			}
			reason += degradedReasonExternalDNSDeleted
			message += degradedReasonExternalDNSDeletedMessage
			// Emit metrics to indicate that external DNS operator is degraded
			metrics.IsExtDNSOperatorDegraded.Set(1)
		} else if externalDNSDeployment.Status.AvailableReplicas == 0 ||
			(externalDNSDeployment.Spec.Replicas != nil && *externalDNSDeployment.Spec.Replicas != externalDNSDeployment.Status.AvailableReplicas) {
			if isReasonPopulated {
				reason += ","
				message += "\n"
			}
			reason += degradedReasonExternalDNSNotAllAvailableReplicas
			message += degradedReasonExternalDNSNotAllAvailableReplicasMessage
			// Emit metrics to indicate that external DNS operator is degraded
			metrics.IsExtDNSOperatorDegraded.Set(1)
		}
	} else {
		metrics.IsExtDNSOperatorDegraded.Set(-1)
	}

	if len(reason) != 0 {
		return metav1.Condition{
			Type:    addonv1alpha1.ManagedClusterAddOnConditionDegraded,
			Status:  metav1.ConditionTrue,
			Reason:  reason,
			Message: message,
		}
	}

	return metav1.Condition{
		Type:    addonv1alpha1.ManagedClusterAddOnConditionDegraded,
		Status:  metav1.ConditionFalse,
		Reason:  degradedReasonHypershiftDeployed,
		Message: degradedReasonHypershiftDeployedMessage,
	}
}

const (
	degradedReasonHypershiftDeployed                 = "HypershiftDeployed"
	degradedReasonHypershiftDeployedMessage          = "Hypershift is deployed on managed cluster."
	degradedReasonOperatorNotFound                   = "OperatorNotFound"
	degradedReasonOperatorDeleted                    = "OperatorDeleted"
	degradedReasonOperatorNotAllAvailableReplicas    = "OperatorNotAllAvailableReplicas"
	degradedReasonExternalDNSNotFound                = "ExternalDNSNotFound"
	degradedReasonExternalDNSDeleted                 = "ExternalDNSDeleted"
	degradedReasonExternalDNSNotAllAvailableReplicas = "ExternalDNSNotAllAvailableReplicas"
)

var (
	degradedReasonOperatorNotFoundMessage                   = fmt.Sprintf("The %s deployment does not exist", util.HypershiftOperatorName)
	degradedReasonOperatorDeletedMessage                    = fmt.Sprintf("The %s deployment is being deleted", util.HypershiftOperatorName)
	degradedReasonOperatorNotAllAvailableReplicasMessage    = fmt.Sprintf("There are no %s replica available", util.HypershiftOperatorName)
	degradedReasonExternalDNSNotFoundMessage                = fmt.Sprintf("The %s deployment does not exist", util.HypershiftOperatorExternalDNSName)
	degradedReasonExternalDNSDeletedMessage                 = fmt.Sprintf("The %s deployment is being deleted", util.HypershiftOperatorExternalDNSName)
	degradedReasonExternalDNSNotAllAvailableReplicasMessage = fmt.Sprintf("There are no %s replica available", util.HypershiftOperatorExternalDNSName)
)
