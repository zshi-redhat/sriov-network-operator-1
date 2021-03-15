/*


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
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/go-logr/logr"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	uns "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sriovnetworkv1 "github.com/k8snetworkplumbingwg/sriov-network-operator/api/v1"
	apply "github.com/k8snetworkplumbingwg/sriov-network-operator/pkg/apply"
	render "github.com/k8snetworkplumbingwg/sriov-network-operator/pkg/render"
	"github.com/k8snetworkplumbingwg/sriov-network-operator/pkg/utils"
	mcfgv1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
)

// SriovOperatorConfigReconciler reconciles a SriovOperatorConfig object
type SriovOperatorConfigReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

var injectorServiceCaCmVersion = ""
var webhookServiceCaCmVersion = ""

// +kubebuilder:rbac:groups=sriovnetwork.openshift.io,resources=sriovoperatorconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sriovnetwork.openshift.io,resources=sriovoperatorconfigs/status,verbs=get;update;patch

func (r *SriovOperatorConfigReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()
	logger := r.Log.WithValues("sriovoperatorconfig", req.NamespacedName)

	logger.Info("Reconciling SriovOperatorConfig")

	enableAdmissionController := os.Getenv("ENABLE_ADMISSION_CONTROLLER") == "true"
	if !enableAdmissionController {
		logger.Info("SR-IOV Network Resource Injector and Operator Webhook are disabled.")
	}
	defaultConfig := &sriovnetworkv1.SriovOperatorConfig{}
	err := r.Get(context.TODO(), types.NamespacedName{
		Name: DEFAULT_CONFIG_NAME, Namespace: namespace}, defaultConfig)
	if err != nil {
		if errors.IsNotFound(err) {
			// Default Config object not found, create it.
			defaultConfig.SetNamespace(namespace)
			defaultConfig.SetName(DEFAULT_CONFIG_NAME)
			defaultConfig.Spec = sriovnetworkv1.SriovOperatorConfigSpec{
				EnableInjector:           func() *bool { b := enableAdmissionController; return &b }(),
				EnableOperatorWebhook:    func() *bool { b := enableAdmissionController; return &b }(),
				ConfigDaemonNodeSelector: map[string]string{},
				LogLevel:                 2,
			}
			err = r.Create(context.TODO(), defaultConfig)
			if err != nil {
				logger.Error(err, "Failed to create default Operator Config", "Namespace",
					namespace, "Name", DEFAULT_CONFIG_NAME)
				return reconcile.Result{}, err
			}
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	if req.Namespace != namespace {
		return reconcile.Result{}, nil
	}

	// Render and sync webhook objects
	if err = r.syncWebhookObjs(defaultConfig); err != nil {
		return reconcile.Result{}, err
	}

	// Sync SriovNetworkConfigDaemon objects
	if err = r.syncConfigDaemonSet(defaultConfig); err != nil {
		return reconcile.Result{}, err
	}

	if err = r.syncPluginDaemonSet(defaultConfig); err != nil {
		return reconcile.Result{}, err
	}

	if utils.ClusterType == utils.ClusterTypeOpenshift {
		if err = r.syncOvsHardwareOffloadMachineConfig(defaultConfig); err != nil {
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{RequeueAfter: ResyncPeriod}, nil
}

func (r *SriovOperatorConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sriovnetworkv1.SriovOperatorConfig{}).
		Owns(&appsv1.DaemonSet{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}

func (r *SriovOperatorConfigReconciler) syncPluginDaemonSet(dc *sriovnetworkv1.SriovOperatorConfig) error {
	logger := r.Log.WithName("syncConfigDaemonset")
	logger.Info("Start to sync SRIOV plugin daemonsets nodeSelector")
	ds := &appsv1.DaemonSet{}

	names := []string{"sriov-cni", "sriov-device-plugin"}

	if len(dc.Spec.ConfigDaemonNodeSelector) == 0 {
		return nil
	}
	for _, name := range names {
		err := r.Client.Get(context.TODO(), types.NamespacedName{Name: name, Namespace: namespace}, ds)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			logger.Error(err, "Couldn't get daemonset", "name", name)
			return err
		}
		ds.Spec.Template.Spec.NodeSelector = dc.Spec.ConfigDaemonNodeSelector
		err = r.Client.Update(context.TODO(), ds)
		if err != nil {
			logger.Error(err, "Couldn't update daemonset", "name", name)
			return err
		}
	}

	return nil
}

func (r *SriovOperatorConfigReconciler) syncConfigDaemonSet(dc *sriovnetworkv1.SriovOperatorConfig) error {
	logger := r.Log.WithName("syncConfigDaemonset")
	logger.Info("Start to sync config daemonset")
	// var err error
	objs := []*uns.Unstructured{}

	data := render.MakeRenderData()
	data.Data["Image"] = os.Getenv("SRIOV_NETWORK_CONFIG_DAEMON_IMAGE")
	data.Data["Namespace"] = namespace
	data.Data["ReleaseVersion"] = os.Getenv("RELEASEVERSION")
	data.Data["ClusterType"] = utils.ClusterType
	objs, err := render.RenderDir(CONFIG_DAEMON_PATH, &data)
	if err != nil {
		logger.Error(err, "Fail to render config daemon manifests")
		return err
	}
	// Sync DaemonSets
	for _, obj := range objs {
		if obj.GetKind() == "DaemonSet" && len(dc.Spec.ConfigDaemonNodeSelector) > 0 {
			scheme := kscheme.Scheme
			ds := &appsv1.DaemonSet{}
			err = scheme.Convert(obj, ds, nil)
			if err != nil {
				logger.Error(err, "Fail to convert to DaemonSet")
				return err
			}
			ds.Spec.Template.Spec.NodeSelector = dc.Spec.ConfigDaemonNodeSelector
			err = scheme.Convert(ds, obj, nil)
			if err != nil {
				logger.Error(err, "Fail to convert to Unstructured")
				return err
			}
		}
		err = r.syncK8sResource(dc, obj)
		if err != nil {
			logger.Error(err, "Couldn't sync SR-IoV daemons objects")
			return err
		}
	}
	return nil
}

func (r *SriovOperatorConfigReconciler) syncWebhookObjs(dc *sriovnetworkv1.SriovOperatorConfig) error {
	logger := r.Log.WithName("syncWebhookObjs")
	logger.Info("Start to sync webhook objects")

	for name, path := range webhooks {
		// Render Webhook manifests
		data := render.MakeRenderData()
		data.Data["Namespace"] = namespace
		data.Data["SRIOVMutatingWebhookName"] = name
		data.Data["NetworkResourcesInjectorImage"] = os.Getenv("NETWORK_RESOURCES_INJECTOR_IMAGE")
		data.Data["SriovNetworkWebhookImage"] = os.Getenv("SRIOV_NETWORK_WEBHOOK_IMAGE")
		data.Data["ReleaseVersion"] = os.Getenv("RELEASEVERSION")
		objs, err := render.RenderDir(path, &data)
		if err != nil {
			logger.Error(err, "Fail to render webhook manifests")
			return err
		}

		// Delete injector webhook
		if *dc.Spec.EnableInjector != true && path == INJECTOR_WEBHOOK_PATH {
			for _, obj := range objs {
				err = r.deleteWebhookObject(obj)
				if err != nil {
					return err
				}
			}
			logger.Info("SR-IOV Admission Controller is disabled.")
			logger.Info("To enable SR-IOV Admission Controller,")
			logger.Info("Set 'SriovOperatorConfig.Spec.EnableInjector' to true(bool).")
			continue
		}
		// Delete operator webhook
		if *dc.Spec.EnableOperatorWebhook != true && path == OPERATOR_WEBHOOK_PATH {
			for _, obj := range objs {
				err = r.deleteWebhookObject(obj)
				if err != nil {
					return err
				}
			}
			logger.Info("Operator Admission Controller is disabled.")
			logger.Info("To enable Operator Admission Controller,")
			logger.Info("Set 'SriovOperatorConfig.Spec.EnableOperatorWebhook' to true(bool).")
			continue
		}

		// Sync Webhook
		for _, obj := range objs {
			err = r.syncWebhookObject(dc, obj)
			if err != nil {
				logger.Error(err, "Couldn't sync webhook objects")
				return err
			}
		}
	}

	return nil
}

func (r *SriovOperatorConfigReconciler) deleteWebhookObject(obj *uns.Unstructured) error {
	if err := r.deleteK8sResource(obj); err != nil {
		return err
	}
	return nil
}

func (r *SriovOperatorConfigReconciler) syncWebhookObject(dc *sriovnetworkv1.SriovOperatorConfig, obj *uns.Unstructured) error {
	var err error
	logger := r.Log.WithName("syncWebhookObject")
	logger.Info("Start to sync Objects")
	scheme := kscheme.Scheme
	switch kind := obj.GetKind(); kind {
	case "MutatingWebhookConfiguration":
		whs := &admissionregistrationv1beta1.MutatingWebhookConfiguration{}
		err = scheme.Convert(obj, whs, nil)
		r.syncMutatingWebhook(dc, whs)
		if err != nil {
			logger.Error(err, "Fail to sync mutate webhook")
			return err
		}
	case "ValidatingWebhookConfiguration":
		whs := &admissionregistrationv1beta1.ValidatingWebhookConfiguration{}
		err = scheme.Convert(obj, whs, nil)
		r.syncValidatingWebhook(dc, whs)
		if err != nil {
			logger.Error(err, "Fail to sync validate webhook")
			return err
		}
	case "ServiceAccount", "DaemonSet", "Service", "ClusterRole", "ClusterRoleBinding":
		err = r.syncK8sResource(dc, obj)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *SriovOperatorConfigReconciler) syncMutatingWebhook(cr *sriovnetworkv1.SriovOperatorConfig, in *admissionregistrationv1beta1.MutatingWebhookConfiguration) error {
	logger := r.Log.WithName("syncMutatingWebhook")
	logger.Info("Start to sync mutating webhook", "Name", in.Name, "Namespace", in.Namespace)

	if err := controllerutil.SetControllerReference(cr, in, r.Scheme); err != nil {
		return err
	}
	whs := &admissionregistrationv1beta1.MutatingWebhookConfiguration{}
	err := r.Get(context.TODO(), types.NamespacedName{Name: in.Name}, whs)
	if err != nil {
		if errors.IsNotFound(err) {
			err = r.Create(context.TODO(), in)
			if err != nil {
				return fmt.Errorf("Couldn't create webhook: %v", err)
			}
			logger.Info("Create webhook for", in.Namespace, in.Name)
		} else {
			return fmt.Errorf("Fail to get webhook: %v", err)
		}
	}

	// Delete deprecated operator mutating webhook CR
	deprecated_webhook := &admissionregistrationv1beta1.MutatingWebhookConfiguration{}
	err = r.Get(context.TODO(), types.NamespacedName{Name: DEPRECATED_OPERATOR_WEBHOOK_NAME}, deprecated_webhook)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		} else {
			logger.Info("Failed to get deprecated operator mutating webhook for", namespace, DEPRECATED_OPERATOR_WEBHOOK_NAME)
		}
	} else {
		err := r.Delete(context.TODO(), deprecated_webhook)
		if err != nil {
			logger.Info("Failed to delete deprecated operator mutating webhook for", namespace, DEPRECATED_OPERATOR_WEBHOOK_NAME)
		} else {
			logger.Info("Deleted deprecated operator mutating webhook for", namespace, DEPRECATED_OPERATOR_WEBHOOK_NAME)
		}
	}

	// Note:
	// we don't need to manage the update of MutatingWebhookConfiguration here
	// as it's handled by caconfig controller

	return nil
}

func (r *SriovOperatorConfigReconciler) syncValidatingWebhook(cr *sriovnetworkv1.SriovOperatorConfig, in *admissionregistrationv1beta1.ValidatingWebhookConfiguration) error {
	logger := r.Log.WithName("syncValidatingWebhook")
	logger.Info("Start to sync validating webhook", "Name", in.Name, "Namespace", in.Namespace)

	if err := controllerutil.SetControllerReference(cr, in, r.Scheme); err != nil {
		return err
	}
	whs := &admissionregistrationv1beta1.ValidatingWebhookConfiguration{}
	err := r.Get(context.TODO(), types.NamespacedName{Name: in.Name}, whs)
	if err != nil {
		if errors.IsNotFound(err) {
			err = r.Create(context.TODO(), in)
			if err != nil {
				return fmt.Errorf("Couldn't create webhook: %v", err)
			}
			logger.Info("Create webhook for", in.Namespace, in.Name)
		} else {
			return fmt.Errorf("Fail to get webhook: %v", err)
		}
	}

	// Delete deprecated operator validating webhook CR
	deprecated_webhook := &admissionregistrationv1beta1.ValidatingWebhookConfiguration{}
	err = r.Get(context.TODO(), types.NamespacedName{Name: DEPRECATED_OPERATOR_WEBHOOK_NAME}, deprecated_webhook)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		} else {
			logger.Info("Failed to get deprecated operator validating webhook for", namespace, DEPRECATED_OPERATOR_WEBHOOK_NAME)
		}
	} else {
		err := r.Delete(context.TODO(), deprecated_webhook)
		if err != nil {
			logger.Info("Failed to delete deprecated operator validating webhook for", namespace, DEPRECATED_OPERATOR_WEBHOOK_NAME)
		} else {
			logger.Info("Deleted deprecated operator validating webhook for", namespace, DEPRECATED_OPERATOR_WEBHOOK_NAME)
		}
	}

	// Note:
	// we don't need to manage the update of MutatingWebhookConfiguration here
	// as it's handled by caconfig controller

	return nil
}

func (r *SriovOperatorConfigReconciler) deleteK8sResource(in *uns.Unstructured) error {
	if err := apply.DeleteObject(context.TODO(), r, in); err != nil {
		return fmt.Errorf("failed to delete object %v with err: %v", in, err)
	}
	return nil
}

func (r *SriovOperatorConfigReconciler) syncK8sResource(cr *sriovnetworkv1.SriovOperatorConfig, in *uns.Unstructured) error {
	// set owner-reference only for namespaced objects
	if in.GetKind() != "ClusterRole" && in.GetKind() != "ClusterRoleBinding" {
		if err := controllerutil.SetControllerReference(cr, in, r.Scheme); err != nil {
			return err
		}
	}
	if err := apply.ApplyObject(context.TODO(), r, in); err != nil {
		return fmt.Errorf("failed to apply object %v with err: %v", in, err)
	}
	return nil
}

func (r *SriovOperatorConfigReconciler) syncOvsHardwareOffloadMachineConfig(dc *sriovnetworkv1.SriovOperatorConfig) error {
	logger := r.Log.WithName("syncOvsHardwareOffloadMachineConfig")
	var err error

	logger.Info("Start to render MachineConfig and MachineConfigPool for OVS HW offloading")

	data := render.MakeRenderData()
	mcpMap := make(map[string]bool)

	mcpList := &mcfgv1.MachineConfigPoolList{}
	err = r.List(context.TODO(), mcpList, &client.ListOptions{})
	if err != nil {
		return fmt.Errorf("Failed to get MachineConfigPoolList: %v", err)
	}

	for _, mcp := range mcpList.Items {
		selector, err := metav1.LabelSelectorAsSelector(mcp.Spec.NodeSelector)
		if err != nil {
			return fmt.Errorf("Invalid label selector in MachineConfigPool: %s, %v", mcp.GetName(), err)
		}
		// Node is selected when its label(s) are included in NodeSelector
		for _, ovsHWOLConfig := range dc.Spec.OvsHardwareOffload {
			if selector.Matches(labels.Set(ovsHWOLConfig.NodeSelector)) {
				// OVS Hardware Offload is not supported on master nodes
				if mcp.GetName() == "master" {
					logger.Info("OVS Hardware Offload is configured on master nodes which is not supported, ignoring.")
					continue
				}
				mcpMap[mcp.GetName()] = true
				break
			}
		}
	}

	for mcpName, enable := range mcpMap {
		mcName := "00-" + mcpName + "-" + OVS_HWOL_MACHINE_CONFIG_NAME_SUFFIX
		mc, err := render.GenerateMachineConfig("bindata/manifests/switchdev-config", mcName, mcpName, dc.Spec.EnableOvsOffload, &data)
		if err != nil {
			return err
		}

		foundMC := &mcfgv1.MachineConfig{}
		err = r.Get(context.TODO(), types.NamespacedName{Name: mcName}, foundMC)
		if err != nil {
			if errors.IsNotFound(err) {
				if dc.Spec.EnableOvsOffload && enable {
					err = r.Create(context.TODO(), mc)
					if err != nil {
						return fmt.Errorf("Couldn't create MachineConfig: %v", err)
					}
					logger.Info("Created MachineConfig CR in MachineConfigPool", mcName, mcpName)
				}
			} else {
				return fmt.Errorf("Failed to get MachineConfig: %v", err)
			}
		} else {
			if dc.Spec.EnableOvsOffload && enable {
				if bytes.Compare(foundMC.Spec.Config.Raw, mc.Spec.Config.Raw) == 0 {
					logger.Info("MachineConfig already exists, updating")
					err = r.Update(context.TODO(), foundMC)
					if err != nil {
						return fmt.Errorf("Couldn't update MachineConfig: %v", err)
					}
				} else {
					logger.Info("No content change, skip updating MC")
				}
			} else {
				logger.Info("offload disabled, delete MachineConfig")
				err = r.Delete(context.TODO(), foundMC)
				if err != nil {
					return fmt.Errorf("Couldn't delete MachineConfig: %v", err)
				}
			}
		}
	}

	nodeOffloadStatus := []sriovnetworkv1.OvsHardwareOffloadConfigStatus{}
	// Remove legacy MCs
	for _, mcp := range mcpList.Items {
		found := false
		for mcpName := range mcpMap {
			if mcp.Name == mcpName {
				nodeOffloadStatus = append(nodeOffloadStatus, sriovnetworkv1.OvsHardwareOffloadConfigStatus{Nodes: []string{mcpName}})
				found = true
				break
			}
		}
		if !found {
			mcName := "00-" + mcp.Name + "-" + OVS_HWOL_MACHINE_CONFIG_NAME_SUFFIX
			foundMC := &mcfgv1.MachineConfig{}
			err = r.Get(context.TODO(), types.NamespacedName{Name: mcName}, foundMC)
			if err != nil {
				if !errors.IsNotFound(err) {
					err = r.Delete(context.TODO(), foundMC)
					if err != nil {
						return fmt.Errorf("Couldn't delete MachineConfig: %v", err)
					}
				}
			}
		}
	}

	// Update SriovOperatorConfig.Status
	if equality.Semantic.DeepDerivative(dc.Status.OvsHardwareOffload, nodeOffloadStatus) {
		dc.Status.OvsHardwareOffload = nodeOffloadStatus
		err = r.Update(context.TODO(), dc)
		if err != nil {
			logger.Error(err, "Fail to update OvsHardwareConfigStatus in default SriovOperatorConfig status")
			return err
		}
	}

	return nil
}
