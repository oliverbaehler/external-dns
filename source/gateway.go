/*
Copyright 2021 The Kubernetes Authors.

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

package source

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"text/template"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	v1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/apis/v1beta1"
	gateway "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	gwinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
	informers_v1beta1 "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions/apis/v1beta1"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/source/annotations"
	"sigs.k8s.io/external-dns/source/fqdn"
	"sigs.k8s.io/external-dns/source/informers"
)

const (
	gatewayGroup = "gateway.networking.k8s.io"
	gatewayKind  = "Gateway"
)

type gatewayRoute interface {
	// Object returns the underlying route object to be used by templates.
	Object() kubeObject
	// Metadata returns the route's metadata.
	Metadata() *metav1.ObjectMeta
	// Hostnames returns the route's specified hostnames.
	Hostnames() []v1.Hostname
	// ParentRefs returns the route's parent references as defined in the route spec.
	ParentRefs() []v1.ParentReference
	// Protocol returns the route's protocol type.
	Protocol() v1.ProtocolType
	// RouteStatus returns the route's common status.
	RouteStatus() v1.RouteStatus
}

type newGatewayRouteInformerFunc func(gwinformers.SharedInformerFactory) gatewayRouteInformer

type gatewayRouteInformer interface {
	List(namespace string, selector labels.Selector) ([]gatewayRoute, error)
	Informer() cache.SharedIndexInformer
}

func newGatewayInformerFactory(client gateway.Interface, namespace string, labelSelector labels.Selector) gwinformers.SharedInformerFactory {
	var opts []gwinformers.SharedInformerOption
	if namespace != "" {
		opts = append(opts, gwinformers.WithNamespace(namespace))
	}
	if labelSelector != nil && !labelSelector.Empty() {
		lbls := labelSelector.String()
		opts = append(opts, gwinformers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = lbls
		}))
	}
	return gwinformers.NewSharedInformerFactoryWithOptions(client, 0, opts...)
}

type gatewayRouteSource struct {
	gwName      string
	gwNamespace string
	gwLabels    labels.Selector
	gwInformer  informers_v1beta1.GatewayInformer

	rtKind        string
	rtNamespace   string
	rtLabels      labels.Selector
	rtAnnotations labels.Selector
	rtInformer    gatewayRouteInformer

	nsInformer coreinformers.NamespaceInformer

	fqdnTemplate             *template.Template
	combineFQDNAnnotation    bool
	ignoreHostnameAnnotation bool
}

func newGatewayRouteSource(clients ClientGenerator, config *Config, kind string, newInformerFn newGatewayRouteInformerFunc) (Source, error) {
	ctx := context.TODO()

	gwLabels, err := getLabelSelector(config.GatewayLabelFilter)
	if err != nil {
		return nil, err
	}
	rtLabels := config.LabelFilter
	if rtLabels == nil {
		rtLabels = labels.Everything()
	}
	rtAnnotations, err := getLabelSelector(config.AnnotationFilter)
	if err != nil {
		return nil, err
	}
	tmpl, err := fqdn.ParseTemplate(config.FQDNTemplate)
	if err != nil {
		return nil, err
	}

	client, err := clients.GatewayClient()
	if err != nil {
		return nil, err
	}

	informerFactory := newGatewayInformerFactory(client, config.GatewayNamespace, gwLabels)
	gwInformer := informerFactory.Gateway().V1beta1().Gateways() // TODO: Gateway informer should be shared across gateway sources.
	gwInformer.Informer()                                        // Register with factory before starting.

	rtInformerFactory := informerFactory
	if config.Namespace != config.GatewayNamespace || !selectorsEqual(rtLabels, gwLabels) {
		rtInformerFactory = newGatewayInformerFactory(client, config.Namespace, rtLabels)
	}
	rtInformer := newInformerFn(rtInformerFactory)
	rtInformer.Informer() // Register with factory before starting.

	kubeClient, err := clients.KubeClient()
	if err != nil {
		return nil, err
	}

	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, 0)
	nsInformer := kubeInformerFactory.Core().V1().Namespaces() // TODO: Namespace informer should be shared across gateway sources.
	nsInformer.Informer()                                      // Register with factory before starting.

	informerFactory.Start(wait.NeverStop)
	kubeInformerFactory.Start(wait.NeverStop)
	if rtInformerFactory != informerFactory {
		rtInformerFactory.Start(wait.NeverStop)

		if err := informers.WaitForCacheSync(ctx, rtInformerFactory); err != nil {
			return nil, err
		}
	}
	if err := informers.WaitForCacheSync(ctx, informerFactory); err != nil {
		return nil, err
	}
	if err := informers.WaitForCacheSync(ctx, kubeInformerFactory); err != nil {
		return nil, err
	}

	src := &gatewayRouteSource{
		gwName:      config.GatewayName,
		gwNamespace: config.GatewayNamespace,
		gwLabels:    gwLabels,
		gwInformer:  gwInformer,

		rtKind:        kind,
		rtNamespace:   config.Namespace,
		rtLabels:      rtLabels,
		rtAnnotations: rtAnnotations,
		rtInformer:    rtInformer,

		nsInformer: nsInformer,

		fqdnTemplate:             tmpl,
		combineFQDNAnnotation:    config.CombineFQDNAndAnnotation,
		ignoreHostnameAnnotation: config.IgnoreHostnameAnnotation,
	}
	return src, nil
}

func (src *gatewayRouteSource) AddEventHandler(ctx context.Context, handler func()) {
	log.Debugf("Adding event handlers for %s", src.rtKind)
	eventHandler := eventHandlerFunc(handler)
	src.gwInformer.Informer().AddEventHandler(eventHandler)
	src.rtInformer.Informer().AddEventHandler(eventHandler)
	src.nsInformer.Informer().AddEventHandler(eventHandler)
}

func (src *gatewayRouteSource) Endpoints(ctx context.Context) ([]*endpoint.Endpoint, error) {
	var endpoints []*endpoint.Endpoint
	routes, err := src.rtInformer.List(src.rtNamespace, src.rtLabels)
	if err != nil {
		return nil, err
	}
	gateways, err := src.gwInformer.Lister().Gateways(src.gwNamespace).List(src.gwLabels)
	if err != nil {
		return nil, err
	}
	namespaces, err := src.nsInformer.Lister().List(labels.Everything())
	if err != nil {
		return nil, err
	}
	kind := strings.ToLower(src.rtKind)
	resolver := newGatewayRouteResolver(src, gateways, namespaces)
	for _, rt := range routes {
		// Filter by annotations.
		meta := rt.Metadata()
		annots := meta.Annotations
		if !src.rtAnnotations.Matches(labels.Set(annots)) {
			continue
		}

		// Check controller annotation to see if we are responsible.
		if v, ok := annots[controllerAnnotationKey]; ok && v != controllerAnnotationValue {
			log.Debugf("Skipping %s %s/%s because controller value does not match, found: %s, required: %s",
				src.rtKind, meta.Namespace, meta.Name, v, controllerAnnotationValue)
			continue
		}

		// Get Route hostnames and their targets.
		hostTargets, err := resolver.resolve(rt)
		if err != nil {
			return nil, err
		}
		if len(hostTargets) == 0 {
			log.Debugf("No endpoints could be generated from %s %s/%s", src.rtKind, meta.Namespace, meta.Name)
			continue
		}

		// Create endpoints from hostnames and targets.
		var routeEndpoints []*endpoint.Endpoint
		resource := fmt.Sprintf("%s/%s/%s", kind, meta.Namespace, meta.Name)
		providerSpecific, setIdentifier := annotations.ProviderSpecificAnnotations(annots)
		ttl := annotations.TTLFromAnnotations(annots, resource)
		for host, targets := range hostTargets {
			routeEndpoints = append(routeEndpoints, EndpointsForHostname(host, targets, ttl, providerSpecific, setIdentifier, resource)...)
		}
		log.Debugf("Endpoints generated from %s %s/%s: %v", src.rtKind, meta.Namespace, meta.Name, routeEndpoints)

		endpoints = append(endpoints, routeEndpoints...)
	}
	return endpoints, nil
}

func namespacedName(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}

type gatewayRouteResolver struct {
	src *gatewayRouteSource
	gws map[types.NamespacedName]gatewayListeners
	nss map[string]*corev1.Namespace
}

type gatewayListeners struct {
	gateway   *v1beta1.Gateway
	listeners map[v1.SectionName][]v1.Listener
}

func newGatewayRouteResolver(src *gatewayRouteSource, gateways []*v1beta1.Gateway, namespaces []*corev1.Namespace) *gatewayRouteResolver {
	// Create Gateway Listener lookup table.
	gws := make(map[types.NamespacedName]gatewayListeners, len(gateways))
	for _, gw := range gateways {
		lss := make(map[v1.SectionName][]v1.Listener, len(gw.Spec.Listeners)+1)
		for i, lis := range gw.Spec.Listeners {
			lss[lis.Name] = gw.Spec.Listeners[i : i+1]
		}
		lss[""] = gw.Spec.Listeners
		gws[namespacedName(gw.Namespace, gw.Name)] = gatewayListeners{
			gateway:   gw,
			listeners: lss,
		}
	}
	// Create Namespace lookup table.
	nss := make(map[string]*corev1.Namespace, len(namespaces))
	for _, ns := range namespaces {
		nss[ns.Name] = ns
	}
	return &gatewayRouteResolver{
		src: src,
		gws: gws,
		nss: nss,
	}
}

func (c *gatewayRouteResolver) resolve(rt gatewayRoute) (map[string]endpoint.Targets, error) {
	rtHosts, err := c.hosts(rt)
	if err != nil {
		return nil, err
	}
	hostTargets := make(map[string]endpoint.Targets)

	routeParentRefs := rt.ParentRefs()

	if len(routeParentRefs) == 0 {
		log.Debugf("No parent references found for %s %s/%s", c.src.rtKind, rt.Metadata().Namespace, rt.Metadata().Name)
		return hostTargets, nil
	}

	meta := rt.Metadata()
	for _, rps := range rt.RouteStatus().Parents {
		// Confirm the Parent is the standard Gateway kind.
		ref := rps.ParentRef
		namespace := strVal((*string)(ref.Namespace), meta.Namespace)
		// Ensure that the parent reference is in the routeParentRefs list
		if !gwRouteHasParentRef(routeParentRefs, ref, meta) {
			log.Debugf("Parent reference %s/%s not found in routeParentRefs for %s %s/%s", namespace, string(ref.Name), c.src.rtKind, meta.Namespace, meta.Name)
			continue
		}

		group := strVal((*string)(ref.Group), gatewayGroup)
		kind := strVal((*string)(ref.Kind), gatewayKind)
		if group != gatewayGroup || kind != gatewayKind {
			log.Debugf("Unsupported parent %s/%s for %s %s/%s", group, kind, c.src.rtKind, meta.Namespace, meta.Name)
			continue
		}
		// Lookup the Gateway and its Listeners.
		gw, ok := c.gws[namespacedName(namespace, string(ref.Name))]
		if !ok {
			log.Debugf("Gateway %s/%s not found for %s %s/%s", namespace, ref.Name, c.src.rtKind, meta.Namespace, meta.Name)
			continue
		}
		// Confirm the Gateway has the correct name, if specified.
		if c.src.gwName != "" && c.src.gwName != gw.gateway.Name {
			log.Debugf("Gateway %s/%s does not match %s %s/%s", namespace, ref.Name, c.src.gwName, meta.Namespace, meta.Name)
			continue
		}

		// Confirm the Gateway has accepted the Route.
		if !gwRouteIsAccepted(rps.Conditions) {
			log.Debugf("Gateway %s/%s has not accepted the current generation %s %s/%s", namespace, ref.Name, c.src.rtKind, meta.Namespace, meta.Name)
			continue
		}

		// Match the Route to all possible Listeners.
		match := false
		section := sectionVal(ref.SectionName, "")
		listeners := gw.listeners[section]
		for i := range listeners {
			lis := &listeners[i]
			// Confirm that the Listener and Route protocols match.
			if !gwProtocolMatches(rt.Protocol(), lis.Protocol) {
				continue
			}
			// Confirm that the Listener and Route ports match, if specified.
			// EXPERIMENTAL: https://gateway-api.sigs.k8s.io/geps/gep-957/
			if ref.Port != nil && *ref.Port != lis.Port {
				continue
			}
			// Confirm that the Listener allows the Route (based on namespace and kind).
			if !c.routeIsAllowed(gw.gateway, lis, rt) {
				continue
			}
			// Find all overlapping hostnames between the Route and Listener.
			// For {TCP,UDP}Routes, all annotation-generated hostnames should match since the Listener doesn't specify a hostname.
			// For {HTTP,TLS}Routes, hostnames (including any annotation-generated) will be required to match any Listeners specified hostname.
			gwHost := ""
			if lis.Hostname != nil {
				gwHost = string(*lis.Hostname)
			}
			for _, rtHost := range rtHosts {
				if gwHost == "" && rtHost == "" {
					// For {HTTP,TLS}Routes, this means the Route and the Listener both allow _any_ hostnames.
					// For {TCP,UDP}Routes, this should always happen since neither specifies hostnames.
					continue
				}
				host, ok := gwMatchingHost(gwHost, rtHost)
				if !ok {
					continue
				}
				override := annotations.TargetsFromTargetAnnotation(gw.gateway.Annotations)
				hostTargets[host] = append(hostTargets[host], override...)
				if len(override) == 0 {
					for _, addr := range gw.gateway.Status.Addresses {
						hostTargets[host] = append(hostTargets[host], addr.Value)
					}
				}
				match = true
			}
		}
		if !match {
			log.Debugf("Gateway %s/%s section %q does not match %s %s/%s hostnames %q", namespace, ref.Name, section, c.src.rtKind, meta.Namespace, meta.Name, rtHosts)
		}
	}
	// If a Gateway has multiple matching Listeners for the same host, then we'll
	// add its IPs to the target list multiple times and should dedupe them.
	for host, targets := range hostTargets {
		hostTargets[host] = uniqueTargets(targets)
	}
	return hostTargets, nil
}

func (c *gatewayRouteResolver) hosts(rt gatewayRoute) ([]string, error) {
	var hostnames []string
	for _, name := range rt.Hostnames() {
		hostnames = append(hostnames, string(name))
	}
	// TODO: The ignore-hostname-annotation flag help says "valid only when using fqdn-template"
	// but other sources don't check if fqdn-template is set. Which should it be?
	if !c.src.ignoreHostnameAnnotation {
		hostnames = append(hostnames, annotations.HostnamesFromAnnotations(rt.Metadata().Annotations)...)
	}
	// TODO: The combine-fqdn-annotation flag is similarly vague.
	if c.src.fqdnTemplate != nil && (len(hostnames) == 0 || c.src.combineFQDNAnnotation) {
		hosts, err := fqdn.ExecTemplate(c.src.fqdnTemplate, rt.Object())
		if err != nil {
			return nil, err
		}
		hostnames = append(hostnames, hosts...)
	}
	// This means that the route doesn't specify a hostname and should use any provided by
	// attached Gateway Listeners. This is only useful for {HTTP,TLS}Routes, but it doesn't
	// break {TCP,UDP}Routes.
	if len(rt.Hostnames()) == 0 {
		hostnames = append(hostnames, "")
	}
	return hostnames, nil
}

func (c *gatewayRouteResolver) routeIsAllowed(gw *v1beta1.Gateway, lis *v1.Listener, rt gatewayRoute) bool {
	meta := rt.Metadata()
	allow := lis.AllowedRoutes

	// Check the route's namespace.
	from := v1.NamespacesFromSame
	if allow != nil && allow.Namespaces != nil && allow.Namespaces.From != nil {
		from = *allow.Namespaces.From
	}
	switch from {
	case v1.NamespacesFromAll:
		// OK
	case v1.NamespacesFromSame:
		if gw.Namespace != meta.Namespace {
			return false
		}
	case v1.NamespacesFromSelector:
		selector, err := metav1.LabelSelectorAsSelector(allow.Namespaces.Selector)
		if err != nil {
			log.Debugf("Gateway %s/%s section %q has invalid namespace selector: %v", gw.Namespace, gw.Name, lis.Name, err)
			return false
		}
		// Get namespace.
		ns, ok := c.nss[meta.Namespace]
		if !ok {
			log.Errorf("Namespace not found for %s %s/%s", c.src.rtKind, meta.Namespace, meta.Name)
			return false
		}
		if !selector.Matches(labels.Set(ns.Labels)) {
			return false
		}
	default:
		log.Debugf("Gateway %s/%s section %q has unknown namespace from %q", gw.Namespace, gw.Name, lis.Name, from)
		return false
	}

	// Check the route's kind, if any are specified by the listener.
	// TODO: Do we need to consider SupportedKinds in the ListenerStatus instead of the Spec?
	// We only support core kinds and already check the protocol... Does this matter at all?
	if allow == nil || len(allow.Kinds) == 0 {
		return true
	}
	gvk := rt.Object().GetObjectKind().GroupVersionKind()
	for _, gk := range allow.Kinds {
		group := strVal((*string)(gk.Group), gatewayGroup)
		if gvk.Group == group && gvk.Kind == string(gk.Kind) {
			return true
		}
	}
	return false
}

func gwRouteHasParentRef(routeParentRefs []v1.ParentReference, ref v1.ParentReference, meta *metav1.ObjectMeta) bool {
	// Ensure that the parent reference is in the routeParentRefs list
	namespace := strVal((*string)(ref.Namespace), meta.Namespace)
	group := strVal((*string)(ref.Group), gatewayGroup)
	kind := strVal((*string)(ref.Kind), gatewayKind)
	for _, rpr := range routeParentRefs {
		rprGroup := strVal((*string)(rpr.Group), gatewayGroup)
		rprKind := strVal((*string)(rpr.Kind), gatewayKind)
		if rprGroup != group || rprKind != kind {
			continue
		}
		rprNamespace := strVal((*string)(rpr.Namespace), meta.Namespace)
		if string(rpr.Name) != string(ref.Name) || rprNamespace != namespace {
			continue
		}
		return true
	}
	return false
}

func gwRouteIsAccepted(conds []metav1.Condition) bool {
	for _, c := range conds {
		if v1.RouteConditionType(c.Type) == v1.RouteConditionAccepted {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

func uniqueTargets(targets endpoint.Targets) endpoint.Targets {
	if len(targets) < 2 {
		return targets
	}
	sort.Strings([]string(targets))
	prev := targets[0]
	n := 1
	for _, v := range targets[1:] {
		if v == prev {
			continue
		}
		prev = v
		targets[n] = v
		n++
	}
	return targets[:n]
}

// gwProtocolMatches returns whether a and b are the same protocol,
// where HTTP and HTTPS are considered the same.
// and TLS and TCP are considered the same.
func gwProtocolMatches(a, b v1.ProtocolType) bool {
	if a == v1.HTTPSProtocolType {
		a = v1.HTTPProtocolType
	}
	if b == v1.HTTPSProtocolType {
		b = v1.HTTPProtocolType
	}
	// if Listener is TLS and Route is TCP set Listener type to TCP as to pass true and return valid match
	if a == v1.TCPProtocolType && b == v1.TLSProtocolType {
		b = v1.TCPProtocolType
	}
	return a == b
}

// gwMatchingHost returns the most-specific overlapping host and a bool indicating if one was found.
// Hostnames that are prefixed with a wildcard label (`*.`) are interpreted as a suffix match.
// That means that "*.example.com" would match both "test.example.com" and "foo.test.example.com",
// but not "example.com". An empty string matches anything.
func gwMatchingHost(a, b string) (string, bool) {
	var ok bool
	if a, ok = gwHost(a); !ok {
		return "", false
	}
	if b, ok = gwHost(b); !ok {
		return "", false
	}

	if a == "" {
		return b, true
	}
	if b == "" || a == b {
		return a, true
	}
	if na, nb := len(a), len(b); nb < na || (na == nb && strings.HasPrefix(b, "*.")) {
		a, b = b, a
	}
	if strings.HasPrefix(a, "*.") && strings.HasSuffix(b, a[1:]) {
		return b, true
	}
	return "", false
}

// gwHost returns the canonical host and a value indicating if it's valid.
func gwHost(host string) (string, bool) {
	if host == "" {
		return "", true
	}
	if isIPAddr(host) || !isDNS1123Domain(strings.TrimPrefix(host, "*.")) {
		return "", false
	}
	return toLowerCaseASCII(host), true
}

// isIPAddr returns whether s in an IP address.
func isIPAddr(s string) bool {
	_, err := netip.ParseAddr(s)
	return err == nil
}

// isDNS1123Domain returns whether s is a valid domain name according to RFC 1123.
func isDNS1123Domain(s string) bool {
	if n := len(s); n == 0 || n > 255 {
		return false
	}
	for lbl, rest := "", s; rest != ""; {
		if lbl, rest, _ = strings.Cut(rest, "."); !isDNS1123Label(lbl) {
			return false
		}
	}
	return true
}

// isDNS1123Label returns whether s is a valid domain label according to RFC 1123.
func isDNS1123Label(s string) bool {
	n := len(s)
	if n == 0 || n > 63 {
		return false
	}
	if !isAlphaNum(s[0]) || !isAlphaNum(s[n-1]) {
		return false
	}
	for i, k := 1, n-1; i < k; i++ {
		if b := s[i]; b != '-' && !isAlphaNum(b) {
			return false
		}
	}
	return true
}

func isAlphaNum(b byte) bool {
	switch {
	case 'a' <= b && b <= 'z',
		'A' <= b && b <= 'Z',
		'0' <= b && b <= '9':
		return true
	default:
		return false
	}
}

func strVal(ptr *string, def string) string {
	if ptr == nil || *ptr == "" {
		return def
	}
	return *ptr
}

func sectionVal(ptr *v1.SectionName, def v1.SectionName) v1.SectionName {
	if ptr == nil || *ptr == "" {
		return def
	}
	return *ptr
}

func selectorsEqual(a, b labels.Selector) bool {
	if a == nil || b == nil {
		return a == b
	}
	aReq, aOK := a.DeepCopySelector().Requirements()
	bReq, bOK := b.DeepCopySelector().Requirements()
	if aOK != bOK || len(aReq) != len(bReq) {
		return false
	}
	sort.Stable(labels.ByKey(aReq))
	sort.Stable(labels.ByKey(bReq))
	for i, r := range aReq {
		if !r.Equal(bReq[i]) {
			return false
		}
	}
	return true
}
