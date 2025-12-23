// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package gateway_api

import (
	"cmp"
	"context"
	"slices"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/cilium/cilium/operator/pkg/model"
	"github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2alpha1"
	"github.com/cilium/cilium/pkg/shortener"
)

const gatewayL4ConfigPrefix = "cilium-gateway-l4-"

type l4BackendKey struct {
	namespace string
	name      string
	port      uint32
}

func (r *gatewayReconciler) ensureGatewayL4Config(ctx context.Context, gw *gatewayv1.Gateway, listeners []model.L4Listener) error {
	desired := r.desiredGatewayL4Config(gw, listeners)
	name := gatewayL4ConfigName(gw)

	if desired == nil {
		existing := &v2alpha1.CiliumGatewayL4Config{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: gw.GetNamespace(), Name: name}, existing); err != nil {
			if k8serrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		return r.Client.Delete(ctx, existing)
	}

	cfg := desired.DeepCopy()
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, cfg, func() error {
		cfg.Spec = desired.Spec
		cfg.OwnerReferences = desired.OwnerReferences
		setMergedLabelsAndAnnotations(cfg, desired)
		return nil
	})
	return err
}

func (r *gatewayReconciler) desiredGatewayL4Config(gw *gatewayv1.Gateway, listeners []model.L4Listener) *v2alpha1.CiliumGatewayL4Config {
	if len(listeners) == 0 {
		return nil
	}

	shortName := shortener.ShortenK8sResourceName(gw.GetName())

	labels := mergeMap(map[string]string{
		owningGatewayLabel: shortName,
		gatewayNameLabel:   shortName,
	}, gw.Labels)

	spec := v2alpha1.CiliumGatewayL4ConfigSpec{
		GatewayRef: v2alpha1.CiliumGatewayReference{
			Name:      gw.GetName(),
			Namespace: gw.GetNamespace(),
		},
	}

	for _, listener := range listeners {
		backendWeights := map[l4BackendKey]int32{}
		for _, route := range listener.Routes {
			for _, be := range route.Backends {
				if be.Port == nil || be.Port.Port == 0 {
					continue
				}

				weight := int32(1)
				if be.Weight != nil {
					weight = *be.Weight
				}
				if weight <= 0 {
					continue
				}

				ns := be.Namespace
				if ns == "" {
					ns = gw.GetNamespace()
				}
				key := l4BackendKey{namespace: ns, name: be.Name, port: be.Port.Port}
				if prev, ok := backendWeights[key]; ok && prev >= weight {
					continue
				}
				backendWeights[key] = weight
			}
		}

		keys := make([]l4BackendKey, 0, len(backendWeights))
		for key := range backendWeights {
			keys = append(keys, key)
		}
		slices.SortFunc(keys, func(a, b l4BackendKey) int {
			if a.namespace != b.namespace {
				return cmp.Compare(a.namespace, b.namespace)
			}
			if a.name != b.name {
				return cmp.Compare(a.name, b.name)
			}
			return cmp.Compare(a.port, b.port)
		})

		backends := make([]v2alpha1.CiliumGatewayL4Backend, 0, len(keys))
		for _, key := range keys {
			weight := backendWeights[key]
			backends = append(backends, v2alpha1.CiliumGatewayL4Backend{
				Name:      key.name,
				Namespace: key.namespace,
				Port:      int32(key.port),
				Weight:    ptr.To(weight),
			})
		}

		spec.Listeners = append(spec.Listeners, v2alpha1.CiliumGatewayL4Listener{
			Name:     listener.Name,
			Protocol: v2alpha1.L4ProtocolType(listener.Protocol),
			Port:     int32(listener.Port),
			Backends: backends,
		})
	}

	return &v2alpha1.CiliumGatewayL4Config{
		ObjectMeta: metav1.ObjectMeta{
			Name:        gatewayL4ConfigName(gw),
			Namespace:   gw.GetNamespace(),
			Labels:      labels,
			Annotations: mergeMap(nil, gw.GetAnnotations()),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: gatewayv1.GroupVersion.String(),
				Kind:       "Gateway",
				Name:       gw.GetName(),
				UID:        gw.GetUID(),
				Controller: ptr.To(true),
			}},
		},
		Spec: spec,
	}
}

func gatewayL4ConfigName(gw *gatewayv1.Gateway) string {
	return shortener.ShortenK8sResourceName(gatewayL4ConfigPrefix + gw.GetName())
}
