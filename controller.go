package main

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	corev1 "k8s.io/api/core/v1"

	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const ConfigMapName = "mesh-map"
const enabled = "enabled"
const MeshSelector = "meshed"
const MeshTimeout = "mesh-timeout"

func newConf(ns string, cfg string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      ConfigMapName,
			Labels: map[string]string{
				MeshSelector: enabled,
			},
		},
		Data: map[string]string{
			"config": cfg,
		},
	}
}

func createManager(ctx context.Context) (manager.Manager, error) {
	var log = logf.Log.WithName("controller")
	log.Info("Starting controller")

	mgr, err := manager.New(config.GetConfigOrDie(), manager.Options{
		MetricsBindAddress: "0",
	})
	if err != nil {
		return nil, err
	}

	selector, err := predicate.LabelSelectorPredicate(metav1.LabelSelector{
		MatchLabels: map[string]string{
			MeshSelector: enabled,
		},
	})
	if err != nil {
		return nil, err
	}
	err = builder.
		ControllerManagedBy(mgr). // Create the ControllerManagedBy
		For(&corev1.ConfigMap{}, builder.WithPredicates(selector)).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
			if object.GetNamespace() == "kube-system" { // Don't to anything for
				return nil
			}
			return []reconcile.Request{
				{types.NamespacedName{Namespace: object.GetNamespace(), Name: ConfigMapName}},
			}
		})).
		Complete(&MeshConfReconciler{
			Client: mgr.GetClient(),
		})
	return mgr, err
}

// MeshConfReconciler is a simple ControllerManagedBy example implementation.
type MeshConfReconciler struct {
	client.Client
}

type ServiceMeta struct {
	Name           string `json:"name"`
	Ip             string `json:"ip"`
	Port           int32  `json:"port"`
	ConnectTimeout string `json:"timeout"`
}

func (a *MeshConfReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := logf.Log.WithName("reconcile").WithValues("namespace", req.Namespace, "name", req.Name)
	log.Info("reconciling")

	services := &corev1.ServiceList{}
	err := a.List(ctx, services, client.InNamespace(req.Namespace), client.MatchingLabels(map[string]string{MeshSelector: enabled}))
	if err != nil {
		return reconcile.Result{}, err
	}

	meshConfig := &corev1.ConfigMap{}
	err = a.Get(ctx, req.NamespacedName, meshConfig)
	if err != nil {
		return reconcile.Result{}, err
	}

	var srvs []ServiceMeta
	for _, s := range services.Items {
		if s.Spec.ClusterIP == "" || len(s.Spec.Ports) == 0 {
			continue
		}
		timeout, err := time.ParseDuration(s.Annotations[MeshTimeout])
		if err != nil {
			return reconcile.Result{}, err
		}
		srvs = append(srvs, ServiceMeta{
			Name:           s.Name,
			Ip:             s.Spec.ClusterIP,
			Port:           s.Spec.Ports[0].Port,
			ConnectTimeout: timeout.String(),
		})
	}
	sort.SliceStable(srvs, func(i, j int) bool {
		return srvs[i].Name < srvs[j].Name
	})

	originConfig := []ServiceMeta{}
	err = json.Unmarshal([]byte(meshConfig.Data["config"]), &originConfig)
	if err != nil {
		return reconcile.Result{}, err
	}
	sort.SliceStable(originConfig, func(i, j int) bool {
		return originConfig[i].Name < originConfig[j].Name
	})
	if compareConf(originConfig, srvs) {
		log.Info("skip configmap update")
		return reconcile.Result{}, nil
	}

	data, err := json.MarshalIndent(srvs, "", "  ")
	if err != nil {
		return reconcile.Result{}, err
	}

	configMap := newConf(req.Namespace, string(data))

	if err = a.Update(ctx, configMap); err != nil && k8s_errors.IsNotFound(err) {
		if cerr := a.Create(ctx, configMap); cerr != nil {
			return reconcile.Result{}, cerr
		}
	} else if err != nil {
		return reconcile.Result{}, err
	}

	log.Info("updated config map")
	return reconcile.Result{}, nil
}

func compareConf(cmp1, cmp2 []ServiceMeta) bool {
	for i := 0; i < len(cmp1); i++ {
		if cmp1[i].Ip != cmp2[i].Ip || cmp1[i].Port != cmp2[i].Port ||
			cmp1[i].Name != cmp2[i].Name || cmp1[i].ConnectTimeout != cmp2[i].ConnectTimeout {
			return false
		}
	}
	return true
}
