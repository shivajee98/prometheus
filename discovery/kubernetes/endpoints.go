// Copyright 2016 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promslog"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/prometheus/prometheus/discovery/targetgroup"
)

// Endpoints discovers new endpoint targets.
type Endpoints struct {
	logger *slog.Logger

	endpointsInf          cache.SharedIndexInformer
	serviceInf            cache.SharedInformer
	podInf                cache.SharedInformer
	nodeInf               cache.SharedInformer
	withNodeMetadata      bool
	namespaceInf          cache.SharedInformer
	withNamespaceMetadata bool

	podStore       cache.Store
	endpointsStore cache.Store
	serviceStore   cache.Store

	queue *workqueue.Type
}

// NewEndpoints returns a new endpoints discovery.
// Endpoints API is deprecated in k8s v1.33+, but we should still support it.
func NewEndpoints(l *slog.Logger, eps cache.SharedIndexInformer, svc, pod, node, namespace cache.SharedInformer, eventCount *prometheus.CounterVec) *Endpoints {
	if l == nil {
		l = promslog.NewNopLogger()
	}

	epAddCount := eventCount.WithLabelValues(RoleEndpoint.String(), MetricLabelRoleAdd)
	epUpdateCount := eventCount.WithLabelValues(RoleEndpoint.String(), MetricLabelRoleUpdate)
	epDeleteCount := eventCount.WithLabelValues(RoleEndpoint.String(), MetricLabelRoleDelete)

	svcAddCount := eventCount.WithLabelValues(RoleService.String(), MetricLabelRoleAdd)
	svcUpdateCount := eventCount.WithLabelValues(RoleService.String(), MetricLabelRoleUpdate)
	svcDeleteCount := eventCount.WithLabelValues(RoleService.String(), MetricLabelRoleDelete)

	podUpdateCount := eventCount.WithLabelValues(RolePod.String(), MetricLabelRoleUpdate)

	e := &Endpoints{
		logger:                l,
		endpointsInf:          eps,
		endpointsStore:        eps.GetStore(),
		serviceInf:            svc,
		serviceStore:          svc.GetStore(),
		podInf:                pod,
		podStore:              pod.GetStore(),
		nodeInf:               node,
		withNodeMetadata:      node != nil,
		namespaceInf:          namespace,
		withNamespaceMetadata: namespace != nil,
		queue:                 workqueue.NewNamed(RoleEndpoint.String()),
	}

	_, err := e.endpointsInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(o interface{}) {
			epAddCount.Inc()
			e.enqueue(o)
		},
		UpdateFunc: func(_, o interface{}) {
			epUpdateCount.Inc()
			e.enqueue(o)
		},
		DeleteFunc: func(o interface{}) {
			epDeleteCount.Inc()
			e.enqueue(o)
		},
	})
	if err != nil {
		l.Error("Error adding endpoints event handler.", "err", err)
	}

	serviceUpdate := func(o interface{}) {
		svc, err := convertToService(o)
		if err != nil {
			e.logger.Error("converting to Service object failed", "err", err)
			return
		}

		obj, exists, err := e.endpointsStore.GetByKey(namespacedName(svc.Namespace, svc.Name))
		if exists && err == nil {
			e.enqueue(obj.(*apiv1.Endpoints))
		}

		if err != nil {
			e.logger.Error("retrieving endpoints failed", "err", err)
		}
	}
	_, err = e.serviceInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		// TODO(fabxc): potentially remove add and delete event handlers. Those should
		// be triggered via the endpoint handlers already.
		AddFunc: func(o interface{}) {
			svcAddCount.Inc()
			serviceUpdate(o)
		},
		UpdateFunc: func(_, o interface{}) {
			svcUpdateCount.Inc()
			serviceUpdate(o)
		},
		DeleteFunc: func(o interface{}) {
			svcDeleteCount.Inc()
			serviceUpdate(o)
		},
	})
	if err != nil {
		l.Error("Error adding services event handler.", "err", err)
	}
	_, err = e.podInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(old, cur interface{}) {
			podUpdateCount.Inc()
			oldPod, ok := old.(*apiv1.Pod)
			if !ok {
				return
			}

			curPod, ok := cur.(*apiv1.Pod)
			if !ok {
				return
			}

			// the Pod's phase may change without triggering an update on the Endpoints/Service.
			// https://github.com/prometheus/prometheus/issues/11305.
			if curPod.Status.Phase != oldPod.Status.Phase {
				e.enqueuePod(namespacedName(curPod.Namespace, curPod.Name))
			}
		},
	})
	if err != nil {
		l.Error("Error adding pods event handler.", "err", err)
	}
	if e.withNodeMetadata {
		_, err = e.nodeInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(o interface{}) {
				node := o.(*apiv1.Node)
				e.enqueueNode(node.Name)
			},
			UpdateFunc: func(_, o interface{}) {
				node := o.(*apiv1.Node)
				e.enqueueNode(node.Name)
			},
			DeleteFunc: func(o interface{}) {
				nodeName, err := nodeName(o)
				if err != nil {
					l.Error("Error getting Node name", "err", err)
				}
				e.enqueueNode(nodeName)
			},
		})
		if err != nil {
			l.Error("Error adding nodes event handler.", "err", err)
		}
	}

	if e.withNamespaceMetadata {
		_, err = e.namespaceInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(_, o interface{}) {
				namespace := o.(*apiv1.Namespace)
				e.enqueueNamespace(namespace.Name)
			},
			// Creation and deletion will trigger events for the change handlers of the resources within the namespace.
			// No need to have additional handlers for them here.
		})
		if err != nil {
			l.Error("Error adding namespaces event handler.", "err", err)
		}
	}

	return e
}

func (e *Endpoints) enqueueNode(nodeName string) {
	endpoints, err := e.endpointsInf.GetIndexer().ByIndex(nodeIndex, nodeName)
	if err != nil {
		e.logger.Error("Error getting endpoints for node", "node", nodeName, "err", err)
		return
	}

	for _, endpoint := range endpoints {
		e.enqueue(endpoint)
	}
}

func (e *Endpoints) enqueueNamespace(namespace string) {
	endpoints, err := e.endpointsInf.GetIndexer().ByIndex(cache.NamespaceIndex, namespace)
	if err != nil {
		e.logger.Error("Error getting endpoints in namespace", "namespace", namespace, "err", err)
		return
	}

	for _, endpoint := range endpoints {
		e.enqueue(endpoint)
	}
}

func (e *Endpoints) enqueuePod(podNamespacedName string) {
	endpoints, err := e.endpointsInf.GetIndexer().ByIndex(podIndex, podNamespacedName)
	if err != nil {
		e.logger.Error("Error getting endpoints for pod", "pod", podNamespacedName, "err", err)
		return
	}

	for _, endpoint := range endpoints {
		e.enqueue(endpoint)
	}
}

func (e *Endpoints) enqueue(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		return
	}

	e.queue.Add(key)
}

// Run implements the Discoverer interface.
func (e *Endpoints) Run(ctx context.Context, ch chan<- []*targetgroup.Group) {
	defer e.queue.ShutDown()

	cacheSyncs := []cache.InformerSynced{e.endpointsInf.HasSynced, e.serviceInf.HasSynced, e.podInf.HasSynced}
	if e.withNodeMetadata {
		cacheSyncs = append(cacheSyncs, e.nodeInf.HasSynced)
	}
	if e.withNamespaceMetadata {
		cacheSyncs = append(cacheSyncs, e.namespaceInf.HasSynced)
	}

	if !cache.WaitForCacheSync(ctx.Done(), cacheSyncs...) {
		if !errors.Is(ctx.Err(), context.Canceled) {
			e.logger.Error("endpoints informer unable to sync cache")
		}
		return
	}

	go func() {
		for e.process(ctx, ch) {
		}
	}()

	// Block until the target provider is explicitly canceled.
	<-ctx.Done()
}

func (e *Endpoints) process(ctx context.Context, ch chan<- []*targetgroup.Group) bool {
	keyObj, quit := e.queue.Get()
	if quit {
		return false
	}
	defer e.queue.Done(keyObj)
	key := keyObj.(string)

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		e.logger.Error("splitting key failed", "key", key)
		return true
	}

	o, exists, err := e.endpointsStore.GetByKey(key)
	if err != nil {
		e.logger.Error("getting object from store failed", "key", key)
		return true
	}
	if !exists {
		send(ctx, ch, &targetgroup.Group{Source: endpointsSourceFromNamespaceAndName(namespace, name)})
		return true
	}
	eps, err := convertToEndpoints(o)
	if err != nil {
		e.logger.Error("converting to Endpoints object failed", "err", err)
		return true
	}
	send(ctx, ch, e.buildEndpoints(eps))
	return true
}

func convertToEndpoints(o interface{}) (*apiv1.Endpoints, error) {
	endpoints, ok := o.(*apiv1.Endpoints)
	if ok {
		return endpoints, nil
	}

	return nil, fmt.Errorf("received unexpected object: %v", o)
}

func endpointsSource(ep *apiv1.Endpoints) string {
	return endpointsSourceFromNamespaceAndName(ep.Namespace, ep.Name)
}

func endpointsSourceFromNamespaceAndName(namespace, name string) string {
	return "endpoints/" + namespace + "/" + name
}

const (
	endpointNodeName               = metaLabelPrefix + "endpoint_node_name"
	endpointHostname               = metaLabelPrefix + "endpoint_hostname"
	endpointReadyLabel             = metaLabelPrefix + "endpoint_ready"
	endpointPortNameLabel          = metaLabelPrefix + "endpoint_port_name"
	endpointPortProtocolLabel      = metaLabelPrefix + "endpoint_port_protocol"
	endpointAddressTargetKindLabel = metaLabelPrefix + "endpoint_address_target_kind"
	endpointAddressTargetNameLabel = metaLabelPrefix + "endpoint_address_target_name"
)

func (e *Endpoints) buildEndpoints(eps *apiv1.Endpoints) *targetgroup.Group {
	tg := &targetgroup.Group{
		Source: endpointsSource(eps),
	}
	tg.Labels = model.LabelSet{
		namespaceLabel: lv(eps.Namespace),
	}
	e.addServiceLabels(eps.Namespace, eps.Name, tg)
	// Add endpoints labels metadata.
	addObjectMetaLabels(tg.Labels, eps.ObjectMeta, RoleEndpoint)

	if e.withNamespaceMetadata {
		tg.Labels = addNamespaceLabels(tg.Labels, e.namespaceInf, e.logger, eps.Namespace)
	}

	type podEntry struct {
		pod          *apiv1.Pod
		servicePorts []apiv1.EndpointPort
	}
	seenPods := map[string]*podEntry{}

	add := func(addr apiv1.EndpointAddress, port apiv1.EndpointPort, ready string) {
		a := net.JoinHostPort(addr.IP, strconv.FormatUint(uint64(port.Port), 10))

		target := model.LabelSet{
			model.AddressLabel:        lv(a),
			endpointPortNameLabel:     lv(port.Name),
			endpointPortProtocolLabel: lv(string(port.Protocol)),
			endpointReadyLabel:        lv(ready),
		}

		if addr.TargetRef != nil {
			target[model.LabelName(endpointAddressTargetKindLabel)] = lv(addr.TargetRef.Kind)
			target[model.LabelName(endpointAddressTargetNameLabel)] = lv(addr.TargetRef.Name)
		}

		if addr.NodeName != nil {
			target[model.LabelName(endpointNodeName)] = lv(*addr.NodeName)
		}
		if addr.Hostname != "" {
			target[model.LabelName(endpointHostname)] = lv(addr.Hostname)
		}

		if e.withNodeMetadata {
			if addr.NodeName != nil {
				target = addNodeLabels(target, e.nodeInf, e.logger, addr.NodeName)
			} else if addr.TargetRef != nil && addr.TargetRef.Kind == "Node" {
				target = addNodeLabels(target, e.nodeInf, e.logger, &addr.TargetRef.Name)
			}
		}

		pod := e.resolvePodRef(addr.TargetRef)
		if pod == nil {
			// This target is not a Pod, so don't continue with Pod specific logic.
			tg.Targets = append(tg.Targets, target)
			return
		}
		s := namespacedName(pod.Namespace, pod.Name)

		sp, ok := seenPods[s]
		if !ok {
			sp = &podEntry{pod: pod}
			seenPods[s] = sp
		}

		// Attach standard pod labels.
		target = target.Merge(podLabels(pod))

		// Attach potential container port labels matching the endpoint port.
		containers := append(pod.Spec.Containers, pod.Spec.InitContainers...)
		for i, c := range containers {
			for _, cport := range c.Ports {
				if port.Port == cport.ContainerPort {
					ports := strconv.FormatUint(uint64(port.Port), 10)
					isInit := i >= len(pod.Spec.Containers)

					target[podContainerNameLabel] = lv(c.Name)
					target[podContainerImageLabel] = lv(c.Image)
					target[podContainerPortNameLabel] = lv(cport.Name)
					target[podContainerPortNumberLabel] = lv(ports)
					target[podContainerPortProtocolLabel] = lv(string(port.Protocol))
					target[podContainerIsInit] = lv(strconv.FormatBool(isInit))
					break
				}
			}
		}

		// Add service port so we know that we have already generated a target
		// for it.
		sp.servicePorts = append(sp.servicePorts, port)
		tg.Targets = append(tg.Targets, target)
	}

	for _, ss := range eps.Subsets {
		for _, port := range ss.Ports {
			for _, addr := range ss.Addresses {
				add(addr, port, "true")
			}
			// Although this generates the same target again, as it was generated in
			// the loop above, it causes the ready meta label to be overridden.
			for _, addr := range ss.NotReadyAddresses {
				add(addr, port, "false")
			}
		}
	}

	v := eps.Labels[apiv1.EndpointsOverCapacity]
	if v == "truncated" {
		e.logger.Warn("Number of endpoints in one Endpoints object exceeds 1000 and has been truncated, please use \"role: endpointslice\" instead", "endpoint", eps.Name)
	}
	if v == "warning" {
		e.logger.Warn("Number of endpoints in one Endpoints object exceeds 1000, please use \"role: endpointslice\" instead", "endpoint", eps.Name)
	}

	// For all seen pods, check all container ports. If they were not covered
	// by one of the service endpoints, generate targets for them.
	for _, pe := range seenPods {
		// PodIP can be empty when a pod is starting or has been evicted.
		if len(pe.pod.Status.PodIP) == 0 {
			continue
		}

		containers := append(pe.pod.Spec.Containers, pe.pod.Spec.InitContainers...)
		for i, c := range containers {
			for _, cport := range c.Ports {
				hasSeenPort := func() bool {
					for _, eport := range pe.servicePorts {
						if cport.ContainerPort == eport.Port {
							return true
						}
					}
					return false
				}
				if hasSeenPort() {
					continue
				}

				a := net.JoinHostPort(pe.pod.Status.PodIP, strconv.FormatUint(uint64(cport.ContainerPort), 10))
				ports := strconv.FormatUint(uint64(cport.ContainerPort), 10)

				isInit := i >= len(pe.pod.Spec.Containers)
				target := model.LabelSet{
					model.AddressLabel:            lv(a),
					podContainerNameLabel:         lv(c.Name),
					podContainerImageLabel:        lv(c.Image),
					podContainerPortNameLabel:     lv(cport.Name),
					podContainerPortNumberLabel:   lv(ports),
					podContainerPortProtocolLabel: lv(string(cport.Protocol)),
					podContainerIsInit:            lv(strconv.FormatBool(isInit)),
				}
				tg.Targets = append(tg.Targets, target.Merge(podLabels(pe.pod)))
			}
		}
	}

	return tg
}

func (e *Endpoints) resolvePodRef(ref *apiv1.ObjectReference) *apiv1.Pod {
	if ref == nil || ref.Kind != "Pod" {
		return nil
	}

	obj, exists, err := e.podStore.GetByKey(namespacedName(ref.Namespace, ref.Name))
	if err != nil {
		e.logger.Error("resolving pod ref failed", "err", err)
		return nil
	}
	if !exists {
		return nil
	}
	return obj.(*apiv1.Pod)
}

func (e *Endpoints) addServiceLabels(ns, name string, tg *targetgroup.Group) {
	obj, exists, err := e.serviceStore.GetByKey(namespacedName(ns, name))
	if err != nil {
		e.logger.Error("retrieving service failed", "err", err)
		return
	}
	if !exists {
		return
	}
	svc := obj.(*apiv1.Service)

	tg.Labels = tg.Labels.Merge(serviceLabels(svc))
}

func addNodeLabels(tg model.LabelSet, nodeInf cache.SharedInformer, logger *slog.Logger, nodeName *string) model.LabelSet {
	if nodeName == nil {
		return tg
	}

	obj, exists, err := nodeInf.GetStore().GetByKey(*nodeName)
	if err != nil {
		logger.Error("Error getting node", "node", *nodeName, "err", err)
		return tg
	}

	if !exists {
		return tg
	}

	node := obj.(*apiv1.Node)
	// Allocate one target label for the node name,
	nodeLabelset := make(model.LabelSet)
	addObjectMetaLabels(nodeLabelset, node.ObjectMeta, RoleNode)
	return tg.Merge(nodeLabelset)
}

func addNamespaceLabels(tg model.LabelSet, namespaceInf cache.SharedInformer, logger *slog.Logger, namespace string) model.LabelSet {
	obj, exists, err := namespaceInf.GetStore().GetByKey(namespace)
	if err != nil {
		logger.Error("Error getting namespace", "namespace", namespace, "err", err)
		return tg
	}

	if !exists {
		return tg
	}

	n := obj.(*apiv1.Namespace)
	namespaceLabelset := make(model.LabelSet)
	addNamespaceMetaLabels(namespaceLabelset, n.ObjectMeta)
	return tg.Merge(namespaceLabelset)
}
